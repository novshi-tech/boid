//go:build windows

package profiles

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFileExclusive is the Windows counterpart of lock_unix.go's flock(2):
// a blocking exclusive lock on f, released by the returned func.
//
// This is a REAL lock, not a stub. `boid login`/`boid logout` on a Windows
// client mutate the same config.yaml (and its sibling token files) that
// MutateConfig's read-modify-write cycle protects, so a no-op here would
// silently reintroduce exactly the lost-update race LockConfigMutation
// exists to prevent — on the one platform where the CLI is the only thing
// running.
//
// LockFileEx locks a byte RANGE rather than a whole file, so the
// convention is to lock a single byte at offset 0; every process reaching
// this file locks the same byte, which makes it whole-file in practice.
// Without LOCKFILE_FAIL_IMMEDIATELY the call blocks until the current
// holder releases, matching LOCK_EX. The lock is released explicitly and
// again implicitly when the caller closes the handle.
func lockFileExclusive(f *os.File) (func(), error) {
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); err != nil {
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
	}, nil
}
