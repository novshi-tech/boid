package kit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestDetect_NilDetect(t *testing.T) {
	dir := t.TempDir()
	k := orchestrator.KitMeta{} // Detect is nil
	if kit.Detect(dir, k) {
		t.Fatal("expected false when Detect is nil")
	}
}

func TestDetect_FilePresent(t *testing.T) {
	dir := t.TempDir()
	// Create a marker file.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Files: []string{"go.mod"}},
	}
	if !kit.Detect(dir, k) {
		t.Fatal("expected true when listed file exists")
	}
}

func TestDetect_FileAbsent(t *testing.T) {
	dir := t.TempDir()
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Files: []string{"go.mod"}},
	}
	if kit.Detect(dir, k) {
		t.Fatal("expected false when no listed file exists")
	}
}

func TestDetect_ORSemantics(t *testing.T) {
	dir := t.TempDir()
	// Only the second file exists.
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Files: []string{"go.mod", "package.json"}},
	}
	if !kit.Detect(dir, k) {
		t.Fatal("expected true: OR semantics — second file exists")
	}
}

func TestDetect_Directory(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Files: []string{".git"}},
	}
	if !kit.Detect(dir, k) {
		t.Fatal("expected true when listed path is a directory")
	}
}
