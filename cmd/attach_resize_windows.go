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
// change (polling, since Windows has no SIGWINCH), and returns an
// idempotent stop func the caller defers. onResize fires only on an actual
// change, not on every tick.
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
