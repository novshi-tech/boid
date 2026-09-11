// Package terminaltitle extracts window titles from a streaming UTF-8 PTY.
package terminaltitle

import (
	"bytes"
	"strings"
	"unicode"
)

const maxTitleSequence = 4096
const (
	ground = iota
	escape
	osc
	oscEscape
	ignoredString
	ignoredEscape
)

// Tracker is a bounded, streaming parser for ESC ] 0/2 title sequences.
// Bytes >= 0x80 are UTF-8 data, never C1 controls (e.g. 本 contains 0x9c).
// One session owns each tracker and calls Write serially.
type Tracker struct {
	state        int
	data         []byte
	overflow     bool
	title, saved string
	seen         bool
	save         func(string) error
}

func New(save func(string) error) *Tracker { return &Tracker{save: save} }

func (t *Tracker) finish() {
	command, value, ok := bytes.Cut(t.data, []byte(";"))
	if !t.overflow && ok && (string(command) == "0" || string(command) == "2") {
		t.title = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, strings.ToValidUTF8(string(value), "")))
		t.seen = true
	}
	t.state = ground
}

func (t *Tracker) escaped(b byte) {
	switch b {
	case ']':
		t.state = osc
		t.data = t.data[:0]
		t.overflow = false
	case 'P', '_', '^', 'X': // DCS, APC, PM, SOS are opaque until ST.
		t.state = ignoredString
	case 0x1b:
		t.state = escape
	default:
		t.state = ground
	}
}

// Write coalesces changes within a chunk and skips repeated titles. Failed
// saves are retried on subsequent output without discarding parser state.
// Oversized and cancelled sequences are ignored; original PTY bytes are intact.
func (t *Tracker) Write(chunk []byte) error {
	for _, b := range chunk {
		if b == 0x18 || b == 0x1a { // CAN / SUB cancel an unfinished sequence.
			t.state = ground
			continue
		}
		switch t.state {
		case ground:
			if b == 0x1b {
				t.state = escape
			}
		case escape:
			t.escaped(b)
		case osc:
			switch b {
			case 0x07:
				t.finish()
			case 0x1b:
				t.state = oscEscape
			default:
				if len(t.data) < maxTitleSequence {
					t.data = append(t.data, b)
				} else {
					t.overflow = true
				}
			}
		case oscEscape:
			if b == '\\' {
				t.finish()
			} else {
				t.escaped(b)
			}
		case ignoredString:
			if b == 0x1b {
				t.state = ignoredEscape
			}
		case ignoredEscape:
			if b == '\\' {
				t.state = ground
			} else if b != 0x1b {
				t.state = ignoredString
			}
		}
	}
	if !t.seen || t.title == t.saved {
		return nil
	}
	if err := t.save(t.title); err != nil {
		return err
	}
	t.saved = t.title
	return nil
}
