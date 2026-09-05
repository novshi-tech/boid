//go:build linux

package api

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// readAttachmentBytes is the Linux TOCTOU-safe attachment reader. It opens
// the leaf name exactly once, relative to an already-open directory
// descriptor, via openat2 with RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS:
//   - RESOLVE_NO_SYMLINKS fails the open (ELOOP) if the leaf is, or resolves
//     through, a symlink at all.
//   - RESOLVE_BENEATH refuses any resolution that would escape dir.
//
// The resulting fd references a fixed inode, so no later swap of the
// directory entry can change what Stat/Read below observe, and
// io.LimitReader enforces the size cap against the live byte stream rather
// than a pre-read Stat size.
func readAttachmentBytes(dir, base string) ([]byte, error) {
	dirFile, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	defer dirFile.Close()

	fd, err := unix.Openat2(int(dirFile.Fd()), base, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			// Kernel predates openat2 (Linux < 5.6) — fall back to the
			// portable best-effort path rather than making the whole
			// attachments feature unavailable on an older host.
			return readAttachmentFilePortable(dir, base)
		}
		// ELOOP (symlink leaf, rejected by RESOLVE_NO_SYMLINKS), EXDEV
		// (RESOLVE_BENEATH would escape dir), ENOENT, etc. all collapse to
		// the same "not found" — never distinguish a traversal/symlink
		// rejection from a plain missing file in the error text.
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir, base))
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("attachment not found: %s", base)
	}

	limited := io.LimitReader(f, AttachmentMaxFileBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	if int64(len(data)) > AttachmentMaxFileBytes {
		return nil, fmt.Errorf("attachment %q exceeds size cap", base)
	}
	return data, nil
}
