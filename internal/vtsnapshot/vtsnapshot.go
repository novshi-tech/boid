// Package vtsnapshot resolves a recorded PTY byte stream into the screen it
// would have painted, so a client attaching mid-session can be handed the
// current screen instead of the whole recording.
//
// A full-screen TUI — Claude Code enters the alternate screen once at startup
// and never leaves it — emits a stream of width-dependent relative cursor
// moves that overdraw the same cells thousands of times. Two things go wrong
// when that raw stream is replayed to a late-joining client:
//
//   - Volume. Measured on a single production job: 8.7 MB of transcript, 39699
//     render frames, median frame 57 bytes. The client paints every
//     intermediate frame — the "the whole session scrolls past on connect"
//     symptom — to arrive at a screen the last frame alone describes.
//   - Correctness. Those relative moves were recorded at one terminal width.
//     Replaying them at another accumulates into garbage, which is why a naive
//     "just replay the tail" bound does not work either: the median frame
//     patches a handful of cells and means nothing without its predecessors.
//
// Resolving the stream through a virtual terminal sized to the recording and
// dumping the resulting grid fixes both: the payload is one screen (plus
// bounded scrollback), and the dump is absolute, so the client's own xterm
// reflows it to whatever width the client actually has.
//
// This is a reinstatement, not a new idea: the same rendering lived in the
// userns backend's runtime_local_linux.go until a47f02bf removed that backend
// wholesale, leaving the container backend replaying raw bytes and go.mod
// carrying an unused x/vt dependency. See docs/plans/web-terminal-vt-emulator.md.
package vtsnapshot

import (
	"io"
	"strings"

	"github.com/charmbracelet/x/vt"
)

// MaxScrollbackLines bounds how many scrolled-off lines a snapshot prepends
// ahead of the visible screen. It exists for the session shape the alternate
// screen does NOT cover — a plain shell, where output scrolls rather than
// overdraws, and where an unbounded dump would be just as large as the raw
// replay it replaces.
//
// Keep aligned with the xterm scrollback in web/static/boid-terminal.js: lines
// beyond what the client's own buffer holds are paid for on the wire and then
// immediately discarded.
const MaxScrollbackLines = 2000

// defaultCols/defaultRows are the geometry a snapshot is resolved at when the
// caller has none to offer. A session adopted after a daemon restart is the
// real case: the PTY size lives in the client's resize frames, and the first
// one arrives after this snapshot has already been sent.
const (
	defaultCols = 80
	defaultRows = 24
)

// Render feeds raw through a virtual terminal of the given geometry and
// returns the resolved screen as a width-independent ANSI dump with SGR styles
// preserved: the most recent MaxScrollbackLines scrolled-off lines, then the
// visible screen. The caller paints it onto a cleared terminal.
//
// Non-positive cols/rows fall back to 80x24. An empty raw returns nil.
//
// Render is CPU-bound and proportional to len(raw); callers must not hold a
// lock across it (the transcript it resolves is appended to by a live read
// loop). Rendering the 8.7 MB job above takes on the order of a tenth of a
// second.
func Render(raw []byte, cols, rows int) []byte {
	if len(raw) == 0 {
		return nil
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = defaultCols, defaultRows
	}

	emu := vt.NewEmulator(cols, rows)

	// The emulator answers device queries (DA1, DSR, XTVERSION, ...) embedded
	// in the recorded output by writing replies to its synchronous input pipe.
	// Nobody consumes those replies here — the real PTY already answered the
	// queries when they were recorded — but the pipe write blocks until
	// drained, so emu.Write below would deadlock without a concurrent reader.
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, emu)
		close(drained)
	}()

	_, _ = emu.Write(raw)

	// Stop the drain reader by closing only the reply pipe's write end, which
	// hands the reader a clean EOF. We deliberately do NOT use emu.Close() for
	// that: it flips an internal `closed` flag the reader checks on every
	// emu.Read, and the race detector (correctly) reports that as a data race
	// inside x/vt. Once <-drained confirms the reader is gone, flipping the
	// flag has no concurrent observer.
	if pw, ok := emu.InputPipe().(*io.PipeWriter); ok {
		_ = pw.Close()
		<-drained
		_ = emu.Close()
	} else {
		// Defensive fallback if x/vt ever changes the pipe type: emu.Close
		// still unblocks the reader, with the data race noted above.
		_ = emu.Close()
		<-drained
	}

	var b strings.Builder
	if sb := emu.Scrollback(); sb != nil {
		lines := sb.Lines()
		if start := len(lines) - MaxScrollbackLines; start > 0 {
			lines = lines[start:]
		}
		for _, ln := range lines {
			b.WriteString(ln.Render())
			b.WriteByte('\n')
		}
	}
	b.WriteString(emu.Render())

	// Both the scrollback loop above and emu.Render join rows with a bare LF.
	// A raw-mode xterm treats LF as line-feed-only (no carriage return), which
	// staircases the dump, so anchor every row at column 0.
	return []byte(strings.ReplaceAll(b.String(), "\n", "\r\n"))
}
