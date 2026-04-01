package orchestrator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestProjectStore_SetGet(t *testing.T) {
	s := orchestrator.NewProjectStore(nil)

	meta := &projectspec.ProjectMeta{
		ID:   "proj-1",
		Name: "Test",
	}
	s.Set("proj-1", meta)

	got, ok := s.Get("proj-1")
	if !ok {
		t.Fatal("expected to find proj-1")
	}
	if got.ID != "proj-1" {
		t.Fatalf("expected id proj-1, got %s", got.ID)
	}
	if got.Name != "Test" {
		t.Fatalf("expected name Test, got %s", got.Name)
	}
}

func TestProjectStore_Get_NotFound(t *testing.T) {
	s := orchestrator.NewProjectStore(nil)

	_, ok := s.Get("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestProjectStore_Remove(t *testing.T) {
	s := orchestrator.NewProjectStore(nil)

	s.Set("proj-1", &projectspec.ProjectMeta{ID: "proj-1", Name: "Test"})
	s.Remove("proj-1")

	_, ok := s.Get("proj-1")
	if ok {
		t.Fatal("expected not found after remove")
	}
}

func TestProjectStore_Load(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: loaded-proj
name: Loaded Project
hooks:
  - id: test-hook
    on: executing
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "test-hook.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	s := orchestrator.NewProjectStore(nil)
	meta, err := s.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if meta.ID != "loaded-proj" {
		t.Fatalf("expected id loaded-proj, got %s", meta.ID)
	}

	got, ok := s.Get("loaded-proj")
	if !ok {
		t.Fatal("expected to find loaded-proj in store after Load")
	}
	if got.Name != "Loaded Project" {
		t.Fatalf("expected name 'Loaded Project', got %s", got.Name)
	}
}

func TestProjectStore_Load_InvalidYAML(t *testing.T) {
	dir := t.TempDir()

	s := orchestrator.NewProjectStore(nil)
	_, err := s.Load(dir)
	if err == nil {
		t.Fatal("expected error for missing project.yaml")
	}
}

func TestProjectStore_LoadAll(t *testing.T) {
	dir1 := t.TempDir()
	setupProjectDir(t, dir1, "proj-a", "Project A")

	dir2 := t.TempDir()
	setupProjectDir(t, dir2, "proj-b", "Project B")

	dir3 := t.TempDir()

	projects := []*projectspec.Project{
		{ID: "proj-a", WorkDir: dir1},
		{ID: "proj-b", WorkDir: dir2},
		{ID: "proj-c", WorkDir: dir3},
	}

	s := orchestrator.NewProjectStore(nil)
	errs := s.LoadAll(projects)

	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}

	if _, ok := s.Get("proj-a"); !ok {
		t.Fatal("expected proj-a to be loaded")
	}
	if _, ok := s.Get("proj-b"); !ok {
		t.Fatal("expected proj-b to be loaded")
	}
}

func TestProjectStore_LoadAll_InvalidatesStaleMetaOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	setupProjectDir(t, dir, "proj-a", "Project A")

	s := orchestrator.NewProjectStore(nil)
	if _, err := s.Load(dir); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	if _, ok := s.Get("proj-a"); !ok {
		t.Fatal("expected proj-a to be loaded before reload")
	}

	boidDir := filepath.Join(dir, ".boid")
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: proj-a\nname: [\n"), 0o644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	errs := s.LoadAll([]*projectspec.Project{{ID: "proj-a", WorkDir: dir}})
	if len(errs) != 1 {
		t.Fatalf("expected 1 reload error, got %d: %v", len(errs), errs)
	}

	if _, ok := s.Get("proj-a"); ok {
		t.Fatal("stale meta should be invalidated after reload failure")
	}
}

func setupProjectDir(t *testing.T, dir, id, name string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := "id: " + id + "\nname: " + name + "\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
}
