//go:build windows

package cmd

import (
	"os"
	"sync"
	"time"
)

// resizePollInterval is how often the Windows resize watcher re-reads the
// console dimensions. 250ms is below the threshold where a human dragging
// a window edge notices the reflow lagging, and the probe itself is a
// single GetConsoleScreenBufferInfo call — cheap enough to run four times
// a second for the lifetime of an attach.
const resizePollInterval = 250 * time.Millisecond

// watchTerminalResize calls onResize whenever the console's dimensions
// change, and returns an idempotent stop func the caller defers.
//
// Windows has no SIGWINCH (see attach_resize_unix.go for the signal-driven
// version), and no console-resize event is reachable through
// golang.org/x/term, so this polls. onResize fires only on an ACTUAL
// change, not on every tick: the remote end would otherwise get four
// pointless resize RPCs per second for the whole session.
func watchTerminalResize(onResize func()) (stop func()) {
	done := make(chan struct{})

	go func() {
		lastRows, lastCols, err := terminalSize(os.Stdout)
		// terminalSize reports (0, 0, nil) when stdout is not a terminal at
		// all, and only returns an error for a real console it could not
		// query — so "no size to watch" is both of those, not just the
		// error. attachLive gates its own initial send on the identical
		// condition (rows > 0 && cols > 0), so exiting here matches it.
		if err != nil || lastRows == 0 || lastCols == 0 {
			return
		}

		ticker := time.NewTicker(resizePollInterval)
		defer ticker.Stop()

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

	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
	}
}
