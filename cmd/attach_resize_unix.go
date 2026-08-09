//go:build !windows

package cmd

import (
	"os"
	"os/signal"
	"syscall"
)

// watchTerminalResize calls onResize every time the terminal window
// changes size, and returns a stop func the caller defers.
//
// SIGWINCH is the kernel telling us the moment it happens — no polling, no
// latency. Split out per GOOS only because Windows has no such signal (see
// attach_resize_windows.go); the behaviour here is what attachLive did
// inline before the split.
func watchTerminalResize(onResize func()) (stop func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-sigCh:
				onResize()
			case <-done:
				return
			}
		}
	}()

	return func() {
		signal.Stop(sigCh)
		close(done)
	}
}
