//go:build linux

package runner

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ensureInitialTTYSize handles engines that start a PTY with zero geometry
// despite ConsoleSize, preserving dimensions already set by an attached client.
func ensureInitialTTYSize() error {
	fd := int(os.Stdin.Fd())
	size, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return fmt.Errorf("read initial terminal size: %w", err)
	}
	if size.Row != 0 && size.Col != 0 {
		return nil
	}
	if size.Row == 0 {
		size.Row = 24
	}
	if size.Col == 0 {
		size.Col = 80
	}
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, size); err != nil {
		return fmt.Errorf("set initial terminal size: %w", err)
	}
	return nil
}
