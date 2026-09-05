//go:build !windows

package cmd

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// watchTerminalResize calls onResize every time the terminal window
// changes size, and returns an idempotent stop func the caller defers that
// actually terminates the watcher goroutine (not just signal.Stop).
// Split out per GOOS since Windows has no SIGWINCH.
func watchTerminalResize(onResize func()) (stop func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-sigCh:
				// Re-check done: a signal buffered before stop() ran can
				// still be selected here even after done closed.
				select {
				case <-done:
					return
				default:
				}
				onResize()
			case <-done:
				return
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(sigCh)
			close(done)
		})
	}
}
