package terminaltitle

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStreamingTitles(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"OSC2 BEL", "before\x1b]2;日本語; title\aafter", []string{"日本語; title"}},
		{"OSC0 ST", "\x1b]0;window\x1b\\", []string{"window"}},
		{"ignore other OSC", "\x1b]1;icon\a\x1b]52;c;clipboard\a", nil},
		{"incomplete", "\x1b]2;unfinished", nil},
		{"deduplicate", "\x1b]2;same\a\x1b]2;same\a", []string{"same"}},
		{"clear", "\x1b]2;title\a\x1b]2;\a", []string{"title", ""}},
		{"cancel", "\x1b]2;cancelled\x18\x1b]2;valid\a", []string{"valid"}},
		{"other string", "\x1bPdata\x1b\\plain", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			tr := New(func(title string) error { got = append(got, title); return nil })
			// Every byte is a distinct PTY chunk, including UTF-8 and ST bytes.
			for i := range len(tc.input) {
				if err := tr.Write([]byte{tc.input[i]}); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTitleCoalescingAndRetry(t *testing.T) {
	var calls []string
	tr := New(func(title string) error {
		calls = append(calls, title)
		if len(calls) == 1 {
			return errors.New("busy")
		}
		return nil
	})
	if err := tr.Write([]byte("\x1b]2;first\a\x1b]2;last\a")); err == nil {
		t.Fatal("expected save error")
	}
	if err := tr.Write([]byte("ordinary output")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Write([]byte("more output\x1b]2;last\a")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"last", "last"}) {
		t.Fatalf("calls: %q", calls)
	}
}

func TestTitleBoundedAndValidUTF8(t *testing.T) {
	var got string
	tr := New(func(title string) error { got = title; return nil })
	if err := tr.Write([]byte("\x1b]2;" + strings.Repeat("日", 100000) + "\a")); err != nil {
		t.Fatal(err)
	}
	if len(got) > 4096 || !utf8.ValidString(got) {
		t.Fatalf("invalid bounded title: %d bytes", len(got))
	}
	if err := tr.Write([]byte("\x1b]2;recovered\a")); err != nil {
		t.Fatal(err)
	}
	if got != "recovered" {
		t.Fatalf("got %q", got)
	}
}
