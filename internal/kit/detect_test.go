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
	if kit.Detect(dir, orchestrator.KitMeta{}) {
		t.Fatal("expected false when Detect is nil")
	}
}

func TestDetect_FilePresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	k := orchestrator.KitMeta{Detect: &orchestrator.KitDetect{Files: []string{"go.mod"}}}
	if !kit.Detect(dir, k) {
		t.Fatal("expected true when go.mod is present")
	}
}

func TestDetect_FileAbsent(t *testing.T) {
	dir := t.TempDir()
	k := orchestrator.KitMeta{Detect: &orchestrator.KitDetect{Files: []string{"go.mod"}}}
	if kit.Detect(dir, k) {
		t.Fatal("expected false when go.mod is absent")
	}
}

func TestDetect_ORSemantics(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Files: []string{"go.mod", "package.json", "Cargo.toml"}},
	}
	if !kit.Detect(dir, k) {
		t.Fatal("expected true: package.json matched (OR semantics)")
	}
}

func TestDetect_Directory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	k := orchestrator.KitMeta{Detect: &orchestrator.KitDetect{Files: []string{".git"}}}
	if !kit.Detect(dir, k) {
		t.Fatal("expected true when .git directory is present")
	}
}
