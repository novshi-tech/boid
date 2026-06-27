package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestBuildProjectStore_AutoPrunesMissingProjectDir pins the auto-prune
// behaviour the boid start parent depends on when a project's work dir has
// been physically deleted (e.g. `/tmp/boid-*-smoke/` test artifacts cleaned
// up out of band).
//
// Before this fix the daemon refused startup with
//
//	daemon startup refused: failed to load project metadata
//	  - project "X": .../.boid/project.yaml: read: open ...: no such file or directory
//	Run `boid project migrate <dir>` ...
//
// and the user had no recovery path: `boid project rm` itself needs the
// daemon running, so the system was stuck. The classification of
// fs.ErrNotExist as ProjectMissingError lets buildProjectStore delete the
// stale DB row and boot anyway. Schema migration errors remain fail-fast.
func TestBuildProjectStore_AutoPrunesMissingProjectDir(t *testing.T) {
	// Isolate the host: workspace store defaults to XDG_CONFIG_HOME/boid/workspaces
	// and EnsureDefault writes a file there. Without this t.Setenv the test would
	// scribble onto the developer's ~/.config/boid.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)

	// 1) Stale project: WorkDir refers to a directory that does not exist.
	missingDir := filepath.Join(t.TempDir(), "vanished")
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID:      "stale-proj",
		WorkDir: missingDir,
	}); err != nil {
		t.Fatalf("create stale project: %v", err)
	}

	// 2) Healthy project: WorkDir has a real .boid/project.yaml so it must
	// still be loaded after the stale row is pruned.
	healthyDir := t.TempDir()
	boidDir := filepath.Join(healthyDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir healthy boid dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"),
		[]byte("id: healthy-proj\nname: Healthy\n"), 0o644); err != nil {
		t.Fatalf("write healthy project.yaml: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID:      "healthy-proj",
		WorkDir: healthyDir,
	}); err != nil {
		t.Fatalf("create healthy project: %v", err)
	}

	cfg := Config{DBPath: ":memory:"}
	store, err := buildProjectStore(cfg, repo)
	if err != nil {
		t.Fatalf("buildProjectStore should boot after auto-prune, got error: %v", err)
	}

	if _, err := repo.GetProject("stale-proj"); err == nil {
		t.Error("stale-proj should have been auto-pruned from DB")
	}
	if _, ok := store.Get("healthy-proj"); !ok {
		t.Error("healthy-proj should still be loaded into the store")
	}
}
