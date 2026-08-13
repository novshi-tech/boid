package vtsnapshot

import (
	"strings"
	"testing"
	"time"
)

func TestRender_Empty(t *testing.T) {
	if got := Render(nil, 80, 24); got != nil {
		t.Fatalf("Render(nil) = %q, want nil", got)
	}
	if got := Render([]byte{}, 80, 24); got != nil {
		t.Fatalf("Render(empty) = %q, want nil", got)
	}
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

	got := Render([]byte(raw.String()), 80, 24)
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
	got := Render([]byte("one\r\ntwo\r\nthree"), 80, 24)
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

	done := make(chan []byte, 1)
	go func() { done <- Render(raw, 80, 24) }()

	select {
	case got := <-done:
		if !strings.Contains(string(got), "after queries") {
			t.Errorf("rendered snapshot dropped post-query output: %q", firstLine(got))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Render deadlocked on a transcript containing device queries")
	}
}

func TestRender_InvalidGeometryFallsBack(t *testing.T) {
	for _, geom := range [][2]int{{0, 0}, {-1, 24}, {80, -1}} {
		got := Render([]byte("hello"), geom[0], geom[1])
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

	got := Render([]byte(raw.String()), 80, 24)
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

	got := Render([]byte(raw.String()), 80, 24)
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

	got := string(Render(raw, 80, 24))
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

	got := string(Render([]byte(raw.String()), 80, 24))
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
