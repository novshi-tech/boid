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
