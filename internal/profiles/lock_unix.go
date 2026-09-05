//go:build !windows

package profiles

import (
	"os"
	"syscall"
)

// lockFileExclusive takes a blocking exclusive advisory lock on f and
// returns the release func — the flock(2) half of LockConfigMutation's
// serialization primitive, split out per GOOS so the CLI still builds for
// GOOS=windows.
//
// LOCK_EX blocks until the holder releases, and release is best-effort
// (the fd is closed right after, which drops the lock anyway).
func lockFileExclusive(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}, nil
}
