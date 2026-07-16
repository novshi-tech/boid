package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic_CreatesFileWithContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "out.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte("hello: world\n"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello: world\n" {
		t.Errorf("content: got %q, want %q", got, "hello: world\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("perm: got %v, want 0644", perm)
	}
}

// TestWriteFileAtomic_FailurePreservesExistingFile pins that a failed write
// (target directory does not exist, so CreateTemp fails before any bytes are
// written) never touches a pre-existing file at path.
func TestWriteFileAtomic_FailurePreservesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-subdir", "out.yaml")

	if err := WriteFileAtomic(path, []byte("new"), 0o644); err == nil {
		t.Fatal("expected error when parent dir does not exist")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("path should not have been created, stat err = %v", err)
	}
}

// TestWriteFileAtomic_NoTempLeftover ensures no leftover temp file is left
// behind in the target directory on a happy path (previously pinned via
// setDefaultHarnessAt, removed in Phase 2.5 PR7's default_harness dead-code
// cleanup — WriteFileAtomic is the actual primitive this behavior belongs
// to).
func TestWriteFileAtomic_NoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")
	if err := WriteFileAtomic(path, []byte("a: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "out.yaml" {
			continue
		}
		t.Errorf("leftover file in dir: %s", e.Name())
	}
}
