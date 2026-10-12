package orchestrator_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// captureSlog redirects the default slog logger to an in-memory buffer for the
// duration of the test. Helper for verifying deprecation warnings.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func writeProjectYAML(t *testing.T, dir, content string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
}
