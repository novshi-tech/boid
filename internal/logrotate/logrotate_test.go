package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// logPath returns <dir>/boid.log
func logPath(dir string) string {
	return filepath.Join(dir, "boid.log")
}

// backupPath returns <dir>/boid.log.<n>
func backupPath(dir string, n int) string {
	return fmt.Sprintf("%s.%d", logPath(dir), n)
}

// writeN writes n bytes to w and asserts no error.
func writeN(t *testing.T, w *Writer, n int) {
	t.Helper()
	buf := make([]byte, n)
	if _, err := w.Write(buf); err != nil {
		t.Fatalf("Write(%d bytes): %v", n, err)
	}
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fileSize returns the size of path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// --- Tests ---

// TestWrite_BasicCreatesFile verifies a new log file is created on first Write.
func TestWrite_BasicCreatesFile(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Path: logPath(dir), MaxSize: 100, MaxBackups: 3}
	defer w.Close()

	writeN(t, w, 10)

	if !fileExists(logPath(dir)) {
		t.Fatal("log file not created after Write")
	}
	if got := fileSize(t, logPath(dir)); got != 10 {
		t.Fatalf("log file size = %d, want 10", got)
	}
}

// TestRotation_RenameOnSizeExceeded verifies that a rotation occurs when
// size + len(p) > MaxSize.
func TestRotation_RenameOnSizeExceeded(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Path: logPath(dir), MaxSize: 50, MaxBackups: 3}
	defer w.Close()

	// Fill to just under the limit.
	writeN(t, w, 45)

	// This write should trigger rotation (45+10 > 50).
	writeN(t, w, 10)

	if !fileExists(backupPath(dir, 1)) {
		t.Fatal("boid.log.1 not created after rotation")
	}
	if !fileExists(logPath(dir)) {
		t.Fatal("boid.log not re-created after rotation")
	}
}

// TestRotation_BackupShift verifies that existing backups are shifted correctly.
// boid.log.1 → boid.log.2 → boid.log.3 across multiple rotations.
func TestRotation_BackupShift(t *testing.T) {
	dir := t.TempDir()
	const maxSize = 10
	w := &Writer{Path: logPath(dir), MaxSize: maxSize, MaxBackups: 5}
	defer w.Close()

	// Rotation 1: boid.log → boid.log.1
	writeN(t, w, 8)
	writeN(t, w, 5) // triggers rotation

	// Rotation 2: boid.log.1 → boid.log.2, boid.log → boid.log.1
	writeN(t, w, 8)
	writeN(t, w, 5)

	// Rotation 3
	writeN(t, w, 8)
	writeN(t, w, 5)

	for i := 1; i <= 3; i++ {
		if !fileExists(backupPath(dir, i)) {
			t.Errorf("boid.log.%d not found after 3 rotations", i)
		}
	}
}

// TestRotation_MaxBackupsDeleted verifies that backups beyond MaxBackups are deleted.
func TestRotation_MaxBackupsDeleted(t *testing.T) {
	dir := t.TempDir()
	const maxSize = 10
	const maxBackups = 3
	w := &Writer{Path: logPath(dir), MaxSize: maxSize, MaxBackups: maxBackups}
	defer w.Close()

	// Perform maxBackups+2 rotations to ensure the oldest backup is dropped.
	for range maxBackups + 2 {
		writeN(t, w, 8)
		writeN(t, w, 5) // triggers rotation
	}

	// Backups 1..maxBackups must exist.
	for i := range maxBackups {
		if !fileExists(backupPath(dir, i+1)) {
			t.Errorf("boid.log.%d missing (should be within MaxBackups)", i+1)
		}
	}
	// Backup maxBackups+1 must NOT exist.
	if fileExists(backupPath(dir, maxBackups+1)) {
		t.Errorf("boid.log.%d exists but should have been deleted (MaxBackups=%d)", maxBackups+1, maxBackups)
	}
}

// TestRotation_ExactBoundary verifies that a write exactly equal to MaxSize
// does not trigger a rotation before writing, but the next byte does.
func TestRotation_ExactBoundary(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Path: logPath(dir), MaxSize: 10, MaxBackups: 3}
	defer w.Close()

	// Write exactly MaxSize bytes — no rotation yet.
	writeN(t, w, 10)
	if fileExists(backupPath(dir, 1)) {
		t.Fatal("boid.log.1 created prematurely (before size exceeded)")
	}

	// One more byte triggers rotation.
	writeN(t, w, 1)
	if !fileExists(backupPath(dir, 1)) {
		t.Fatal("boid.log.1 not created after exceeding MaxSize")
	}
}

// TestConcurrentWrite verifies that concurrent writes do not corrupt the
// writer state and all writes are accepted without error.
func TestConcurrentWrite_Safe(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Path: logPath(dir), MaxSize: 512, MaxBackups: 3}
	defer w.Close()

	const goroutines = 20
	const writesPerG = 50
	const chunkSize = 7 // small chunks to provoke many rotations

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*writesPerG)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, chunkSize)
			for range writesPerG {
				if _, err := w.Write(buf); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent Write error: %v", err)
	}
}

// TestClose_Idempotent verifies that Close can be called multiple times without error.
func TestClose_Idempotent(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Path: logPath(dir), MaxSize: 100, MaxBackups: 3}

	writeN(t, w, 5)

	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestWrite_AfterClose_ReOpens verifies that writing after Close re-opens the file.
func TestWrite_AfterClose_ReOpens(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Path: logPath(dir), MaxSize: 100, MaxBackups: 3}

	writeN(t, w, 5)
	w.Close()

	// Write after close should re-open.
	writeN(t, w, 5)
	defer w.Close()

	if got := fileSize(t, logPath(dir)); got != 10 {
		t.Fatalf("log file size = %d after reopen, want 10", got)
	}
}

// TestDefaultMaxSize verifies the default MaxSize is applied when MaxSize <= 0.
func TestDefaultMaxSize(t *testing.T) {
	w := &Writer{}
	if got := w.maxSize(); got != DefaultMaxSize {
		t.Fatalf("maxSize() = %d, want %d", got, DefaultMaxSize)
	}
}

// TestDefaultMaxBackups verifies the default MaxBackups is applied when MaxBackups <= 0.
func TestDefaultMaxBackups(t *testing.T) {
	w := &Writer{}
	if got := w.maxBackups(); got != DefaultMaxBackups {
		t.Fatalf("maxBackups() = %d, want %d", got, DefaultMaxBackups)
	}
}
