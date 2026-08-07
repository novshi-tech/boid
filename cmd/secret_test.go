package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretStdinIsInteractive_RegularFileIsNotInteractive pins the fix for
// the onboarding-dogfood finding that `boid secret set K < file` fell through
// to the interactive prompt: the old ModeNamedPipe test only recognised a
// pipe, so a redirected regular file — the shape every restore script uses —
// looked like a terminal and the command sat waiting for a human.
func TestSecretStdinIsInteractive_RegularFileIsNotInteractive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if secretStdinIsInteractive(stat) {
		t.Error("a redirected regular file must be read as piped input, not prompted for")
	}
}

// TestSecretStdinIsInteractive_PipeIsNotInteractive keeps the shape that
// already worked (`printf ... | boid secret set K`) working.
func TestSecretStdinIsInteractive_PipeIsNotInteractive(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	stat, err := r.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if secretStdinIsInteractive(stat) {
		t.Error("a pipe must be read as piped input, not prompted for")
	}
}

// TestSecretStdinIsInteractive_CharDeviceIsInteractive is the other half of
// the contract: a character device (what a real terminal is) still gets the
// prompt. /dev/null stands in for a tty here — it is a character device, and
// the mode bit is all secretStdinIsInteractive looks at.
func TestSecretStdinIsInteractive_CharDeviceIsInteractive(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !secretStdinIsInteractive(stat) {
		t.Error("a character device must still be prompted for")
	}
}

func TestReadPipedSecretValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		raw   bool
		want  string
	}{
		{
			name:  "single trailing newline is stripped",
			input: "s3cret\n",
			want:  "s3cret",
		},
		{
			// The old bufio.Scanner implementation split on lines and
			// rejoined with "\n", which silently dropped every trailing
			// newline rather than just the one the shell adds.
			name:  "only one trailing newline is stripped",
			input: "s3cret\n\n",
			want:  "s3cret\n",
		},
		{
			name:  "no trailing newline is left alone",
			input: "s3cret",
			want:  "s3cret",
		},
		{
			name:  "interior newlines survive (PEM-shaped values)",
			input: "-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----\n",
			want:  "-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----",
		},
		{
			name:  "raw keeps the trailing newline",
			input: "s3cret\n",
			raw:   true,
			want:  "s3cret\n",
		},
		{
			name:  "raw keeps every trailing newline",
			input: "s3cret\n\n\n",
			raw:   true,
			want:  "s3cret\n\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readPipedSecretValue(strings.NewReader(tt.input), tt.raw)
			if err != nil {
				t.Fatalf("readPipedSecretValue: %v", err)
			}
			if got != tt.want {
				t.Errorf("readPipedSecretValue(%q, raw=%v) = %q, want %q", tt.input, tt.raw, got, tt.want)
			}
		})
	}
}
