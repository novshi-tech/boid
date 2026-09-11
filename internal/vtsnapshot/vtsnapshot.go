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
// reflows it to whatever width the client actually has. See
// docs/plans/web-terminal-vt-emulator.md.
package vtsnapshot

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
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

// Render resolves a transcript into a styled screen and bounded scrollback,
// returning emulator failures as errors so callers can replay raw output.
func Render(raw []byte, cols, rows int) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = defaultCols, defaultRows
	}

	prefix, tail := splitAtSafeBoundary(raw)
	emu := vt.NewEmulator(cols, rows)
	// x/vt's parser has the same UTF-8 transition behavior as ansi.Parser:
	// after a non-ASCII rune in an OSC it can dispatch the rune as printable
	// text. Title OSCs are metadata, so omit completed OSC 0/1/2 sequences from
	// the emulator input while preserving every other control sequence.
	out, err := render(stripCompletedTitleOSC(prefix), emu)
	if err != nil {
		return nil, err
	}
	return append(out, stripCompletedTitleOSC(tail)...), nil
}

func stripCompletedTitleOSC(raw []byte) []byte {
	// Keep the output non-nil from the start. A nil output used to be treated
	// as the "nothing removed" sentinel, which lost bytes after a title OSC at
	// offset zero and returned raw (including the title) at the end.
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); {
		if i+4 < len(raw) && raw[i] == 0x1B && raw[i+1] == ']' &&
			(raw[i+2] == '0' || raw[i+2] == '1' || raw[i+2] == '2') && raw[i+3] == ';' {
			end := i + 4
			completed := false
			utf8Remaining := 0
			for end < len(raw) {
				if utf8Remaining > 0 {
					utf8Remaining--
					end++
					continue
				}
				if raw[end] >= 0xC2 && raw[end] <= 0xDF {
					utf8Remaining = 1
				} else if raw[end] >= 0xE0 && raw[end] <= 0xEF {
					utf8Remaining = 2
				} else if raw[end] >= 0xF0 && raw[end] <= 0xF4 {
					utf8Remaining = 3
				}
				if raw[end] == 0x07 || raw[end] == 0x9C {
					completed = true
					break
				}
				if raw[end] == 0x1B && end+1 < len(raw) && raw[end+1] == '\\' {
					completed = true
					break
				}
				// CAN/SUB cancel the string. An ESC that is not ST starts a
				// new escape sequence; neither case completes this OSC. Keep
				// the bytes rather than swallowing intervening screen text up
				// to a later BEL/ST.
				if raw[end] == 0x18 || raw[end] == 0x1A || raw[end] == 0x1B {
					break
				}
				end++
			}
			if completed {
				i = end + 1
				if raw[end] == 0x1B {
					i++
				}
				continue
			}
		}
		out = append(out, raw[i])
		i++
	}
	return out
}

// splitAtSafeBoundary keeps parser state that has not reached ground state in
// the replay stream. This matters for strings, escape sequences, and split
// UTF-8 runes: rendering only their prefix would make the next live bytes be
// interpreted as ordinary text by the attaching terminal.
func splitAtSafeBoundary(raw []byte) (prefix, tail []byte) {
	p := ansi.NewParser()
	last := 0
	state := boundaryGround
	utf8Remaining := 0
	for i, b := range raw {
		p.Advance(b)
		state, utf8Remaining = advanceBoundary(state, utf8Remaining, b)
		// The ansi parser returns to GroundState after a complete UTF-8 rune,
		// even when that rune was inside an OSC/DCS string. The small scanner
		// above preserves that enclosing string state for the boundary decision.
		if p.State() == parser.GroundState && state == boundaryGround && utf8Remaining == 0 {
			last = i + 1
		}
	}
	return raw[:last], raw[last:]
}

type boundaryState uint8

const (
	boundaryGround boundaryState = iota
	boundaryEscape
	boundaryCSI
	boundaryString
	boundaryStringEscape
)

// advanceBoundary tracks the parser context that ansi.Parser cannot retain
// across a completed UTF-8 rune in a string sequence.
func advanceBoundary(state boundaryState, remaining int, b byte) (boundaryState, int) {
	if remaining > 0 {
		return state, remaining - 1
	}
	if b >= 0xC2 && b <= 0xDF {
		return state, 1
	}
	if b >= 0xE0 && b <= 0xEF {
		return state, 2
	}
	if b >= 0xF0 && b <= 0xF4 {
		return state, 3
	}
	if state == boundaryString {
		if b == 0x07 || b == 0x9C {
			return boundaryGround, 0
		}
		if b == 0x1B {
			return boundaryStringEscape, 0
		}
		if b == 0x18 || b == 0x1A {
			return boundaryGround, 0
		}
		return state, 0
	}
	switch state {
	case boundaryGround:
		if b == 0x1B {
			return boundaryEscape, 0
		}
	case boundaryEscape:
		switch b {
		case '[':
			return boundaryCSI, 0
		case ']', 'P', '^', '_':
			return boundaryString, 0
		default:
			return boundaryGround, 0
		}
	case boundaryCSI:
		if b >= 0x40 && b <= 0x7E {
			return boundaryGround, 0
		}
		if b == 0x1B {
			return boundaryEscape, 0
		}
		if b == 0x18 || b == 0x1A {
			return boundaryGround, 0
		}
	case boundaryStringEscape:
		if b == '\\' {
			return boundaryGround, 0
		}
		if b == 0x1B {
			return boundaryStringEscape, 0
		}
		return boundaryString, 0
	}
	return state, 0
}

func render(raw []byte, emu *vt.Emulator) (output []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			output = nil
			err = fmt.Errorf("render terminal snapshot: %v", recovered)
		}
	}()

	// Handlers run newest first. Clamp parser-backed margins to prevent
	// out-of-bounds scrolling when replaying output from a larger screen.
	emu.RegisterCsiHandler('r', func(params ansi.Params) bool {
		clampMargin(params, emu.Height())
		return false
	})
	emu.RegisterCsiHandler('s', func(params ansi.Params) bool {
		clampMargin(params, emu.Width())
		return false
	})

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

	defer func() {
		// Close the write end and join the reader before emu.Close changes
		// the emulator's closed flag, avoiding a race with emu.Read.
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
	}()

	_, _ = emu.Write(raw)
	// Preserve the recorded cursor, rather than leaving it at the dump's end.
	cursor := emu.CursorPosition()

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
	dump := strings.ReplaceAll(b.String(), "\n", "\r\n")

	// This CUP is correct only when the connecting client's own viewport has
	// exactly `rows` rows: cursor.Y is a row index into the screen this
	// function rendered, and the client counts rows from the top of what it
	// painted (scrollback lines then screen), so the two only line up at
	// matching row counts. cols/rows is the geometry of the LAST client that
	// resized this session, not necessarily the one now attaching — a wrong
	// client geometry means a wrong cursor row/col, an open axis noted in
	// docs/plans/web-terminal-vt-emulator.md.
	dump += fmt.Sprintf("\x1b[%d;%dH", cursor.Y+1, cursor.X+1)

	return []byte(dump), nil
}

// clampMargin bounds the far margin, preserving missing/zero defaults.
// The default handler rejects a near margin at or beyond the far margin.
func clampMargin(params ansi.Params, size int) {
	far, more, ok := params.Param(1, size)
	if ok && far > size {
		params[1] = ansi.Param(ansi.Parameter(size, more))
	}
}
