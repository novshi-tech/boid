package project_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/project"
)

func TestStore_SetGet(t *testing.T) {
	s := project.NewStore(nil)

	meta := &model.ProjectMeta{
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

func TestStore_Get_NotFound(t *testing.T) {
	s := project.NewStore(nil)

	_, ok := s.Get("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestStore_Remove(t *testing.T) {
	s := project.NewStore(nil)

	s.Set("proj-1", &model.ProjectMeta{ID: "proj-1", Name: "Test"})
	s.Remove("proj-1")

	_, ok := s.Get("proj-1")
	if ok {
		t.Fatal("expected not found after remove")
	}
}

func TestStore_Load(t *testing.T) {
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

	s := project.NewStore(nil)
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

func TestStore_Load_InvalidYaml(t *testing.T) {
	dir := t.TempDir()

	s := project.NewStore(nil)
	_, err := s.Load(dir)
	if err == nil {
		t.Fatal("expected error for missing project.yaml")
	}
}

func TestStore_LoadAll(t *testing.T) {
	// Create two valid project directories
	dir1 := t.TempDir()
	setupProjectDir(t, dir1, "proj-a", "Project A")

	dir2 := t.TempDir()
	setupProjectDir(t, dir2, "proj-b", "Project B")

	// Create one invalid project directory (no project.yaml)
	dir3 := t.TempDir()

	projects := []*model.Project{
		{ID: "proj-a", WorkDir: dir1},
		{ID: "proj-b", WorkDir: dir2},
		{ID: "proj-c", WorkDir: dir3},
	}

	s := project.NewStore(nil)
	errs := s.LoadAll(projects)

	// Should have 1 error (for proj-c)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}

	// proj-a and proj-b should be loaded
	if _, ok := s.Get("proj-a"); !ok {
		t.Fatal("expected proj-a to be loaded")
	}
	if _, ok := s.Get("proj-b"); !ok {
		t.Fatal("expected proj-b to be loaded")
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
