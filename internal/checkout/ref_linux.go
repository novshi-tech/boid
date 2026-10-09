//go:build linux

package checkout

import (
	"os"
	"syscall"
)

func openCacheRefFile(path string) (*os.File, error) {
	// Avoid blocking on a FIFO swapped in after Lstat, and never follow symlinks.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
