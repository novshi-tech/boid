//go:build linux

package client

import (
	"fmt"
	"os"
	"syscall"
)

// lockAutostart takes the exclusive advisory lock that serializes the
// autostart critical section (see ensureRunning), returning the release
// func the caller defers. Split out per GOOS so internal/client compiles
// on non-Linux targets — flock(2) is the only non-portable call on the
// whole client path.
func lockAutostart(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("acquire autostart lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}, nil
}
