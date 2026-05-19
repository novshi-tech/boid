package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"
)

// ReadTranscript reads the transcript.log for the given runtimeID from rootDir.
// Returns os.ErrNotExist if the transcript file does not exist (e.g. runtime was gc'd).
func ReadTranscript(rootDir, runtimeID string) ([]byte, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("rootDir is required")
	}
	if runtimeID == "" {
		return nil, fmt.Errorf("runtimeID is required")
	}
	path := filepath.Join(rootDir, runtimeID, localRuntimeTranscriptFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// StatTranscript returns os.FileInfo for the transcript.log of the given runtimeID.
// Returns os.ErrNotExist if the file does not exist (e.g. runtime was gc'd).
func StatTranscript(rootDir, runtimeID string) (os.FileInfo, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("rootDir is required")
	}
	if runtimeID == "" {
		return nil, fmt.Errorf("runtimeID is required")
	}
	path := filepath.Join(rootDir, runtimeID, localRuntimeTranscriptFile)
	return os.Stat(path)
}
