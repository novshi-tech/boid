//go:build windows

package cmd

import (
	"os"
	"time"
)

// resizePollInterval is how often the Windows resize watcher re-reads the
// console dimensions. 250ms is below the threshold where a human dragging
// a window edge notices the reflow lagging, and the probe itself is a
// single GetConsoleScreenBufferInfo call — cheap enough to run four times
// a second for the lifetime of an attach.
const resizePollInterval = 250 * time.Millisecond

// watchTerminalResize calls onResize whenever the console's dimensions
// change, and returns a stop func the caller defers.
//
// Windows has no SIGWINCH (see attach_resize_unix.go for the signal-driven
// version), and no portable console-event API reachable through
// golang.org/x/term, so this polls. onResize fires only on an ACTUAL
// change, not on every tick: the remote end would otherwise get four
// pointless resize RPCs per second for the whole session.
//
// The dimensions are read here rather than inside onResize because the
// watcher needs them to detect the change at all; onResize (attachLive's
// sendResize) reads them again when it fires. That double read is
// deliberate — it keeps this function's contract identical to the Unix
// one, whose callback likewise takes no arguments.
func watchTerminalResize(onResize func()) (stop func()) {
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(resizePollInterval)
		defer ticker.Stop()

		lastRows, lastCols, err := terminalSize(os.Stdout)
		if err != nil {
			// Not a console we can measure — nothing to watch. attachLive
			// has already sent the initial size (or skipped it on the same
			// error), so exiting here is the correct no-op.
			return
		}

		for {
			select {
			case <-ticker.C:
				rows, cols, err := terminalSize(os.Stdout)
				if err != nil || (rows == lastRows && cols == lastCols) {
					continue
				}
				lastRows, lastCols = rows, cols
				onResize()
			case <-done:
				return
			}
		}
	}()

	return func() { close(done) }
}
