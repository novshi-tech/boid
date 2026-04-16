package orchestrator_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestWriteProjectLocalMeta(t *testing.T) {
	dir := t.TempDir()

	meta := &projectspec.ProjectLocalMeta{
		Env: map[string]string{"FOO": "bar"},
	}
	if err := projectspec.WriteProjectLocalMeta(dir, meta); err != nil {
		t.Fatalf("WriteProjectLocalMeta: %v", err)
	}

	path := filepath.Join(dir, ".boid", "project.local.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read project.local.yaml: %v", err)
	}
	if !strings.Contains(string(data), "version: 1") {
		t.Fatalf("expected version header, got %s", data)
	}

	loaded, err := projectspec.ReadProjectLocalMeta(dir)
	if err != nil {
		t.Fatalf("ReadProjectLocalMeta: %v", err)
	}
	if loaded.Env["FOO"] != "bar" {
		t.Fatalf("unexpected loaded meta: %+v", loaded)
	}
}
