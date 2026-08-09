//go:build !windows

package cmd

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// watchTerminalResize calls onResize every time the terminal window
// changes size, and returns a stop func the caller defers.
//
// SIGWINCH is the kernel telling us the moment it happens — no polling, no
// latency. Split out per GOOS only because Windows has no such signal (see
// attach_resize_windows.go).
//
// The stop func is idempotent and actually terminates the watcher
// goroutine. The version this replaced (inline in attachLive) only called
// signal.Stop, which stops delivery but does not close the channel — so
// its `for range sigCh` goroutine blocked forever, leaking one goroutine
// per attach.
func watchTerminalResize(onResize func()) (stop func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-sigCh:
				// A signal that arrived before stop() ran is still buffered
				// and can be chosen even now that done is closed — select
				// picks uniformly among ready cases. Without this re-check
				// that would fire one resize RPC after attachLive already
				// returned and restored the terminal.
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
