package orchestrator_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestProjectStore_SetGet(t *testing.T) {
	s := orchestrator.NewProjectStore()

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
	s := orchestrator.NewProjectStore()

	_, ok := s.Get("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestProjectStore_Remove(t *testing.T) {
	s := orchestrator.NewProjectStore()

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
task_behaviors:
  dev:
    name: dev
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	_ = hooksDir

	s := orchestrator.NewProjectStore()
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

// TestProjectStore_Load_ProjectLocalIgnored verifies that the deprecated
// project.local.yaml is silently ignored; its env/host_commands/additional_bindings
// are no longer merged into behaviors. Users should migrate to workspace.yaml.
func TestProjectStore_Load_ProjectLocalIgnored(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: loaded-proj\nname: Loaded Project\ntask_behaviors:\n  dev:\n    name: dev\n"), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("env:\n  LOCAL: yes\n"), 0o644); err != nil {
		t.Fatalf("write project.local.yaml: %v", err)
	}

	s := orchestrator.NewProjectStore()
	meta, err := s.Load(dir)
	if err != nil {
		t.Fatalf("load with project.local.yaml present: %v", err)
	}
	// project.local.yaml is deprecated and must not be merged.
	if meta.TaskBehaviors["dev"].Env["LOCAL"] == "yes" {
		t.Fatalf("expected project.local.yaml env to be ignored, but LOCAL=yes was applied")
	}
}

func TestProjectStore_Load_InvalidYAML(t *testing.T) {
	dir := t.TempDir()

	s := orchestrator.NewProjectStore()
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

	s := orchestrator.NewProjectStore()
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

// TestProjectStore_LoadAll_MissingDirReturnsTypedError pins the
// classification that lets the boid start parent / server wire auto-prune
// stale project DB rows instead of refusing daemon startup. When the
// candidate's WorkDir has no .boid/project.yaml on disk, LoadAll must
// return a *ProjectMissingError (not a plain fmt.Errorf wrap), so callers
// can errors.As it out and decide to delete the row.
//
// This separates the "dir physically gone" case (data-safe to prune) from
// other load failures (parse errors, permission errors) that remain
// fail-fast because they can mask real config bugs.
func TestProjectStore_LoadAll_MissingDirReturnsTypedError(t *testing.T) {
	dir1 := t.TempDir()
	setupProjectDir(t, dir1, "proj-a", "Project A")

	// dir2 exists but has no .boid/project.yaml — simulates a project whose
	// directory was wiped (e.g. test artifact under /tmp).
	dir2 := t.TempDir()

	s := orchestrator.NewProjectStore()
	errs := s.LoadAll([]*projectspec.Project{
		{ID: "proj-a", WorkDir: dir1},
		{ID: "proj-b", WorkDir: dir2},
	})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error (proj-b missing), got %d: %v", len(errs), errs)
	}
	var missErr *orchestrator.ProjectMissingError
	if !errors.As(errs[0], &missErr) {
		t.Fatalf("expected *ProjectMissingError for missing dir, got %T: %v", errs[0], errs[0])
	}
	if missErr.ProjectID != "proj-b" {
		t.Errorf("ProjectID = %q, want %q", missErr.ProjectID, "proj-b")
	}
	if missErr.Dir != dir2 {
		t.Errorf("Dir = %q, want %q", missErr.Dir, dir2)
	}
	// errors.Is(fs.ErrNotExist) must still resolve through Unwrap so callers
	// that key off the standard sentinel keep working.
	if !errors.Is(missErr, os.ErrNotExist) {
		t.Errorf("errors.Is(*ProjectMissingError, os.ErrNotExist) = false, want true")
	}
	// The healthy project should still be loaded.
	if _, ok := s.Get("proj-a"); !ok {
		t.Error("proj-a should be loaded despite proj-b being missing")
	}
}

func TestProjectStore_LoadAll_InvalidatesStaleMetaOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	setupProjectDir(t, dir, "proj-a", "Project A")

	s := orchestrator.NewProjectStore()
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
