// Package logrotate provides a size-based rotating log writer.
// It implements io.Writer and io.Closer, safe for concurrent use.
package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	// DefaultMaxSize is the default maximum log file size before rotation (10 MiB).
	DefaultMaxSize = 10 * 1024 * 1024
	// DefaultMaxBackups is the default number of backup files to retain.
	DefaultMaxBackups = 5
)

// Writer is an io.WriteCloser that writes to Path and rotates the file when
// its size exceeds MaxSize.  Backup files are named Path.1, Path.2, …
// At most MaxBackups backup files are kept; older ones are deleted.
type Writer struct {
	// Path is the absolute path to the active log file.
	Path string
	// MaxSize is the maximum file size in bytes before rotation.
	// Zero or negative uses DefaultMaxSize.
	MaxSize int64
	// MaxBackups is the number of backup files to retain.
	// Zero or negative uses DefaultMaxBackups.
	MaxBackups int

	mu   sync.Mutex
	file *os.File
	size int64
}

// Write implements io.Writer.  It rotates the underlying file when
// the current size plus len(p) would exceed MaxSize.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}

	if w.size+int64(len(p)) > w.maxSize() {
		if err := w.rotateLocked(); err != nil {
			// On rotate failure fall back to stderr; do not stop the writer.
			fmt.Fprintf(os.Stderr, "logrotate: rotate failed: %v\n", err)
			// If we lost the file handle, we cannot write.
			if w.file == nil {
				return 0, err
			}
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// openLocked opens (or creates) the log file and records its current size.
// Caller must hold w.mu.
func (w *Writer) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
		return fmt.Errorf("logrotate: create dir: %w", err)
	}
	f, err := os.OpenFile(w.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("logrotate: open: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("logrotate: stat: %w", err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

// rotateLocked performs the rotation:
//  1. Close the current log file.
//  2. Shift existing backup files (Path.N → Path.N+1, delete if > MaxBackups).
//  3. Rename Path → Path.1.
//  4. Open a fresh Path.
//
// Caller must hold w.mu.
func (w *Writer) rotateLocked() error {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}

	mb := w.maxBackups()

	// Shift backups from highest index down to 1.
	for i := mb; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.Path, i)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		if i == mb {
			// At limit — delete rather than shift beyond MaxBackups.
			os.Remove(src) //nolint:errcheck
		} else {
			dst := fmt.Sprintf("%s.%d", w.Path, i+1)
			os.Rename(src, dst) //nolint:errcheck
		}
	}

	// Rename current log → .1
	if _, err := os.Stat(w.Path); err == nil {
		if err := os.Rename(w.Path, w.Path+".1"); err != nil {
			return fmt.Errorf("logrotate: rename log: %w", err)
		}
	}

	return w.openLocked()
}

func (w *Writer) maxSize() int64 {
	if w.MaxSize <= 0 {
		return DefaultMaxSize
	}
	return w.MaxSize
}

func (w *Writer) maxBackups() int {
	if w.MaxBackups <= 0 {
		return DefaultMaxBackups
	}
	return w.MaxBackups
}
