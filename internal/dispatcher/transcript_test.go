package dispatcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadTranscript(t *testing.T) {
	rootDir := t.TempDir()
	runtimeID := "test-runtime-id"
	runtimeDir := filepath.Join(rootDir, runtimeID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	content := "hello from transcript\n"
	transcriptPath := filepath.Join(runtimeDir, localRuntimeTranscriptFile)
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	got, err := ReadTranscript(rootDir, runtimeID)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if string(got) != content {
		t.Errorf("ReadTranscript() = %q, want %q", string(got), content)
	}
}

func TestReadTranscript_NotFound(t *testing.T) {
	rootDir := t.TempDir()

	_, err := ReadTranscript(rootDir, "nonexistent-runtime")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadTranscript() error = %v, want os.ErrNotExist", err)
	}
}

func TestReadTranscript_EmptyArgs(t *testing.T) {
	if _, err := ReadTranscript("", "some-id"); err == nil {
		t.Error("ReadTranscript('', 'some-id') should return error")
	}
	if _, err := ReadTranscript("/tmp", ""); err == nil {
		t.Error("ReadTranscript('/tmp', '') should return error")
	}
}

func TestStatTranscript(t *testing.T) {
	rootDir := t.TempDir()
	runtimeID := "stat-runtime-id"
	runtimeDir := filepath.Join(rootDir, runtimeID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	content := "hello stat\n"
	transcriptPath := filepath.Join(runtimeDir, localRuntimeTranscriptFile)
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	fi, err := StatTranscript(rootDir, runtimeID)
	if err != nil {
		t.Fatalf("StatTranscript() error = %v", err)
	}
	if fi.Size() != int64(len(content)) {
		t.Errorf("Size() = %d, want %d", fi.Size(), len(content))
	}
	if fi.ModTime().IsZero() {
		t.Error("ModTime() should not be zero")
	}
}

func TestStatTranscript_NotFound(t *testing.T) {
	rootDir := t.TempDir()

	_, err := StatTranscript(rootDir, "nonexistent-runtime")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("StatTranscript() error = %v, want os.ErrNotExist", err)
	}
}

func TestStatTranscript_EmptyArgs(t *testing.T) {
	if _, err := StatTranscript("", "some-id"); err == nil {
		t.Error("StatTranscript('', 'some-id') should return error")
	}
	if _, err := StatTranscript("/tmp", ""); err == nil {
		t.Error("StatTranscript('/tmp', '') should return error")
	}
}
