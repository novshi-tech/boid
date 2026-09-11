package vtsnapshot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

func TestRender_Empty(t *testing.T) {
	if got := mustRender(t, nil, 80, 24); got != nil {
		t.Fatalf("Render(nil) = %q, want nil", got)
	}
	if got := mustRender(t, []byte{}, 80, 24); got != nil {
		t.Fatalf("Render(empty) = %q, want nil", got)
	}
}

func TestRender_PreservesIncompleteParserTail(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, tail string
	}{
		{"osc-bel", "prompt> \x1b]0;", "TEST TITLE"},
		{"osc-st", "prompt> \x1b]0;TEST TITLE", "\x1b"},
		{"csi", "prompt> \x1b[", "31"},
		{"dcs", "prompt> \x1bP1;", "2;3+"},
		{"utf8", "prompt> ", "\xe3\x81"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.prefix + tc.tail)
			got := mustRender(t, raw, 80, 24)
			if !strings.HasSuffix(string(got), tc.tail) {
				t.Fatalf("snapshot = %q, want original parser tail %q at end", got, tc.tail)
			}
		})
	}
}

// TestRender_ReplaysParserBoundaryInVendoredXterm exercises the actual client
// parser. A snapshot may end at every byte of an OSC (including a split UTF-8
// title); the rendered prefix and untouched tail must still compose into the
// same title without painting the title text or producing input data.
func TestRender_ReplaysParserBoundaryInVendoredXterm(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("vendored xterm regression test requires node: %v", err)
	}
	xtermPath, err := filepath.Abs("../../web/static/assets/xterm-5.x/xterm.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(xtermPath); err != nil {
		t.Fatal(err)
	}

	const runner = `
const { Terminal } = require(process.env.BOID_XTERM_JS);
const snapshot = Buffer.from(process.argv[1], 'base64');
const suffix = Buffer.from(process.argv[2], 'base64');
const term = new Terminal({cols: 80, rows: 24, scrollback: 100});
const titles = [], data = [];
term.onTitleChange(title => titles.push(title));
term.onData(value => data.push(value));
term.write(snapshot, () => term.write(suffix, () => {
  const lines = [];
  for (let row = 0; row < term.rows; row++) {
    lines.push(term.buffer.active.getLine(row)?.translateToString(true) || '');
  }
  process.stdout.write(JSON.stringify({titles, data, screen: lines.join('\n')}));
}));
`
	tests := []struct {
		name, title, terminator string
	}{
		{"ascii-bel", "TEST TITLE", "\x07"},
		{"ascii-st", "TEST TITLE", "\x1b\\"},
		{"utf8-bel", "日本語タイトル", "\x07"},
		{"utf8-st", "日本語タイトル", "\x1b\\"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sequence := []byte("\x1b]0;" + tc.title + tc.terminator)
			for split := 0; split <= len(sequence); split++ {
				raw := append([]byte("prompt> "), sequence[:split]...)
				snapshot := mustRender(t, raw, 80, 24)
				live := sequence[split:]
				result := runXterm(t, node, xtermPath, runner, snapshot, live)
				if len(result.Data) != 0 {
					t.Fatalf("split %d: onData = %q, want empty", split, result.Data)
				}
				if len(live) != 0 && (len(result.Titles) == 0 || result.Titles[len(result.Titles)-1] != tc.title) {
					t.Fatalf("split %d: titles = %q, want final title %q", split, result.Titles, tc.title)
				}
				if strings.TrimRight(result.Screen, " \n") != "prompt>" {
					t.Fatalf("split %d: screen = %q, want exactly prompt", split, result.Screen)
				}
			}
		})
	}
}

func TestRender_TitleAtStartAndCancelledOSC(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("vendored xterm regression test requires node: %v", err)
	}
	xtermPath, err := filepath.Abs("../../web/static/assets/xterm-5.x/xterm.js")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, raw, want string
	}{
		{"ascii-bel", "\x1b]0;TITLE\x07prompt> ", "prompt>"},
		{"unicode-bel", "\x1b]0;日本語タイトル\x07prompt> ", "prompt>"},
		{"ascii-st", "\x1b]0;TITLE\x1b\\prompt> ", "prompt>"},
		{"consecutive", "\x1b]0;FIRST\x07\x1b]0;SECOND\x07prompt> ", "prompt>"},
		{"cancel-can", "\x1b]0;discarded\x18visible\x07prompt> ", "visibleprompt>"},
		{"cancel-sub", "\x1b]0;discarded\x1asurvives\x07prompt> ", "survivesprompt>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := mustRender(t, []byte(tc.raw), 80, 24)
			if strings.Contains(string(snapshot), "TITLE") || strings.Contains(string(snapshot), "タイトル") || strings.Contains(string(snapshot), "FIRST") || strings.Contains(string(snapshot), "SECOND") {
				t.Fatalf("title leaked into snapshot: %q", snapshot)
			}
			result := runXterm(t, node, xtermPath, titleReplayRunner, snapshot, nil)
			if len(result.Data) != 0 {
				t.Fatalf("onData = %q, want empty", result.Data)
			}
			if !strings.Contains(result.Screen, tc.want) {
				t.Fatalf("screen = %q, want %q", result.Screen, tc.want)
			}
		})
	}
}

const titleReplayRunner = `
const { Terminal } = require(process.env.BOID_XTERM_JS);
const snapshot = Buffer.from(process.argv[1], 'base64');
const suffix = Buffer.from(process.argv[2], 'base64');
const term = new Terminal({cols: 80, rows: 24, scrollback: 100});
const data = [];
term.onData(value => data.push(value));
term.write(snapshot, () => term.write(suffix, () => {
  const lines = [];
  for (let row = 0; row < term.rows; row++) {
    lines.push(term.buffer.active.getLine(row)?.translateToString(true) || '');
  }
  process.stdout.write(JSON.stringify({titles: [], data, screen: lines.join('\n')}));
}));
`

func TestRender_UnicodeDoesNotDisableSnapshotCompaction(t *testing.T) {
	raw := []byte(strings.Repeat("\x1b[H日本語の画面", 1000))
	got := mustRender(t, raw, 80, 24)
	if len(got) >= 1000 {
		t.Fatalf("Unicode transcript was not compacted: got %d bytes from %d", len(got), len(raw))
	}

	raw = []byte("\x1b]0;日本語タイトル\x07" + strings.Repeat("\x1b[H画面", 1000))
	got = mustRender(t, raw, 80, 24)
	if len(got) >= 1000 {
		t.Fatalf("Unicode title transcript was not compacted: got %d bytes from %d", len(got), len(raw))
	}
	if strings.Contains(string(got), "日本語タイトル") {
		t.Fatalf("completed title leaked into rendered snapshot: %q", got)
	}
}

type xtermReplayResult struct {
	Titles []string `json:"titles"`
	Data   []string `json:"data"`
	Screen string   `json:"screen"`
}

func runXterm(t *testing.T, node, xtermPath, runner string, snapshot, suffix []byte) xtermReplayResult {
	t.Helper()
	encode := func(value []byte) string { return base64.StdEncoding.EncodeToString(value) }
	cmd := exec.Command(node, "-e", runner, encode(snapshot), encode(suffix))
	cmd.Env = append(os.Environ(), "BOID_XTERM_JS="+xtermPath)
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("xterm replay: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("xterm replay: %v", err)
	}
	var result xtermReplayResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode xterm replay result %q: %v", out, err)
	}
	return result
}

// TestRender_ResolvesOverdrawnCells is the whole point of this package: a TUI
// paints the same cells over and over, and replaying that raw stream costs the
// client every intermediate frame. The rendered snapshot must carry only the
// final state.
func TestRender_ResolvesOverdrawnCells(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("\x1b[?1049h") // alt screen, as Claude Code does
	for i := 0; i < 500; i++ {
		raw.WriteString("\x1b[H")
		raw.WriteString("frame")
	}
	raw.WriteString("\x1b[H")
	raw.WriteString("final")

	got := mustRender(t, []byte(raw.String()), 80, 24)
	if !strings.Contains(string(got), "final") {
		t.Fatalf("rendered snapshot lost the last frame: %q", firstLine(got))
	}
	if strings.Contains(string(got), "frame") {
		t.Errorf("rendered snapshot still carries an overdrawn earlier frame: %q", firstLine(got))
	}
	if len(got) >= len(raw.String()) {
		t.Errorf("rendered snapshot (%d bytes) did not shrink the raw stream (%d bytes)", len(got), raw.Len())
	}
}

// TestRender_RowsAreCRLFTerminated pins the raw-mode xterm requirement: the
// emulator joins rows with a bare LF, which a raw-mode terminal treats as
// line-feed-only and staircases. Every LF must carry a CR.
func TestRender_RowsAreCRLFTerminated(t *testing.T) {
	got := mustRender(t, []byte("one\r\ntwo\r\nthree"), 80, 24)
	s := string(got)
	if !strings.Contains(s, "\r\n") {
		t.Fatalf("no CRLF in rendered snapshot: %q", s)
	}
	for i, r := range s {
		if r == '\n' && (i == 0 || s[i-1] != '\r') {
			t.Fatalf("bare LF at byte %d of rendered snapshot: %q", i, s)
		}
	}
}

// TestRender_DeviceQueryDoesNotDeadlock covers the trap the first
// implementation hit: the emulator answers DA1/DSR/XTVERSION by writing to its
// own input pipe, and that write blocks until something drains it — so a
// transcript containing a device query would hang Render forever.
func TestRender_DeviceQueryDoesNotDeadlock(t *testing.T) {
	raw := []byte("\x1b[c\x1b[5n\x1b[>q" + "after queries")

	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := Render(raw, 80, 24)
		done <- result{output, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !strings.Contains(string(got.output), "after queries") {
			t.Errorf("rendered snapshot dropped post-query output: %q", firstLine(got.output))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Render deadlocked on a transcript containing device queries")
	}
}

func TestRender_InvalidGeometryFallsBack(t *testing.T) {
	for _, geom := range [][2]int{{0, 0}, {-1, 24}, {80, -1}} {
		got := mustRender(t, []byte("hello"), geom[0], geom[1])
		if !strings.Contains(string(got), "hello") {
			t.Errorf("Render(cols=%d, rows=%d) lost its input: %q", geom[0], geom[1], got)
		}
	}
}

// TestRender_ScrollbackIsBounded keeps a long non-alt-screen session (a plain
// shell, where every line scrolls off into the emulator's scrollback rather
// than being overdrawn) from reintroducing the unbounded payload this package
// exists to remove.
func TestRender_ScrollbackIsBounded(t *testing.T) {
	var raw strings.Builder
	for i := 0; i < MaxScrollbackLines*3; i++ {
		raw.WriteString("line\r\n")
	}

	got := mustRender(t, []byte(raw.String()), 80, 24)
	lines := strings.Count(string(got), "\r\n") + 1
	if lines > MaxScrollbackLines+24+2 {
		t.Errorf("rendered snapshot kept %d lines, want at most scrollback cap %d plus one screen", lines, MaxScrollbackLines)
	}
	if lines < 24 {
		t.Errorf("rendered snapshot kept only %d lines, want at least the visible screen", lines)
	}
}

// TestRender_KeepsScrollbackHistory pins the other half of the bound: capping
// is not truncating to the viewport. A session that scrolled a little must
// still hand the client the scrolled-off lines to scroll back through.
func TestRender_KeepsScrollbackHistory(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("NEEDLE\r\n")
	for i := 0; i < 60; i++ {
		raw.WriteString("filler\r\n")
	}

	got := mustRender(t, []byte(raw.String()), 80, 24)
	if !strings.Contains(string(got), "NEEDLE") {
		t.Error("rendered snapshot dropped a scrolled-off line that is still within the cap")
	}
}

// TestRender_RestoresCursorPosition pins the fix for attach landing the
// client's cursor at the end of the dump instead of where the recorded
// session actually left it: a mid-prompt cursor position must survive the
// resolve as a trailing absolute Cursor Position (CUP) escape.
func TestRender_RestoresCursorPosition(t *testing.T) {
	// Move to row 3 (0-indexed), column 5, then leave the cursor there —
	// nothing after it should move it again.
	raw := []byte("\x1b[4;6Hprompt> ")

	got := string(mustRender(t, raw, 80, 24))
	want := "\x1b[4;14H" // row 3 -> "4", column 5+len("prompt> ")=13 -> "14", both 1-indexed
	if !strings.HasSuffix(got, want) {
		t.Fatalf("rendered snapshot does not end with cursor restore %q: %q", want, got)
	}
}

// TestRender_RestoresCursorPositionWithScrollback pins the assumption
// TestRender_RestoresCursorPosition alone leaves untested: cursor.Y is a
// SCREEN-relative row (0..rows-1), not offset by however many scrollback
// lines were prepended ahead of it. A session that scrolled must still land
// the CUP within the trailing `rows`-line screen block, not somewhere inside
// the scrollback block above it.
func TestRender_RestoresCursorPositionWithScrollback(t *testing.T) {
	var raw strings.Builder
	for i := 0; i < 60; i++ {
		raw.WriteString("filler\r\n")
	}
	raw.WriteString("END") // no trailing CRLF: cursor stays right after it

	got := string(mustRender(t, []byte(raw.String()), 80, 24))
	want := "\x1b[24;4H" // last screen row (24, 1-indexed) x column len("END")+1
	if !strings.HasSuffix(got, want) {
		t.Fatalf("rendered snapshot does not end with cursor restore %q: %q", want, got)
	}
}

func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func mustRender(t *testing.T, raw []byte, cols, rows int) []byte {
	t.Helper()
	got, err := Render(raw, cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRender_ScrollMarginsAfterResize(t *testing.T) {
	for _, tc := range []struct {
		name, raw, bounded string
	}{
		{"reverse index", "\x1b[1;81r\x1b[H\x1bM", "\x1b[1;5r\x1b[H\x1bM"},
		{"insert lines", "\x1b[1;81r\x1b[H\x1b[2L", "\x1b[1;5r\x1b[H\x1b[2L"},
		{"delete lines", "\x1b[1;81r\x1b[H\x1b[2M", "\x1b[1;5r\x1b[H\x1b[2M"},
		{"horizontal margins", "\x1b[?69h\x1b[1;291s\x1b[H\x1bM", "\x1b[?69h\x1b[1;10s\x1b[H\x1bM"},
		{"near margin outside screen", "\x1b[70;81r\x1b[H\x1bM", "\x1b[H\x1bM"},
		{"default near margin", "\x1b[;81r\x1b[H\x1bM", "\x1b[1;5r\x1b[H\x1bM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "one\r\ntwo\r\nthree\r\nfour\r\nfive"
			suffix := "\x1b[Hlive"
			got := mustRender(t, []byte(prefix+tc.raw+suffix), 10, 5)
			want := mustRender(t, []byte(prefix+tc.bounded+suffix), 10, 5)
			if string(got) != string(want) {
				t.Fatalf("snapshot = %q, want bounded screen %q", got, want)
			}
			if !strings.Contains(string(got), "live") {
				t.Fatalf("lost output after scrolling: %q", got)
			}
		})
	}
}

func TestRender_PanicReturnsErrorAndClosesReplyPipe(t *testing.T) {
	emu := vt.NewEmulator(10, 5)
	emu.RegisterCsiHandler('z', func(ansi.Params) bool { panic("broken emulator") })
	output, err := render([]byte("\x1b[c\x1b[z"), emu)
	if err == nil || !strings.Contains(err.Error(), "broken emulator") || output != nil {
		t.Fatalf("render = %q, %v; want no output and panic error", output, err)
	}
	// render joins the reply-draining goroutine before returning, even on
	// panic. A closed pipe also proves future writes cannot leave it blocked.
	if _, err := emu.InputPipe().Write([]byte("reply")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("reply pipe write error = %v, want closed pipe", err)
	}
}
