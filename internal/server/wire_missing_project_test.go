package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestBuildProjectStore_DegradesMissingProjectDir_NeverDeletes pins the
// docs/plans/volume-only-daemon.md §論点a on-startup-auto-prune-retirement
// invariant, replacing this file's former
// TestBuildProjectStore_AutoPrunesMissingProjectDir (which pinned the
// OPPOSITE behavior — auto-deleting the DB row — that this same doc
// declares dangerous: it is the exact mechanism that turned a transient
// "container can't see the host filesystem" observation into the
// 2026-07-24 dogfood incident's mass deletion of 18 projects / 479 tasks /
// 816 jobs, which is what prompted the doc's pivot in the first place).
//
// Before that original auto-prune existed the daemon refused startup with
//
//	daemon startup refused: failed to load project metadata
//	  - project "X": .../.boid/project.yaml: read: open ...: no such file or directory
//	Run `boid project migrate <dir>` ...
//
// and the user had no recovery path (`boid project rm` itself needs the
// daemon running). §論点a's answer is not "go back to refusing startup" —
// it is "boot anyway, but never delete": a project whose project.yaml
// fails to load is marked StatusDegraded (in-memory, via
// ProjectStore.MarkDegraded) and its DB row is left completely alone.
// Recovery is still available (`boid project rm` once the daemon is up),
// it's just no longer automatic or silent.
func TestBuildProjectStore_DegradesMissingProjectDir_NeverDeletes(t *testing.T) {
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
	// still be loaded alongside the degraded one.
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
	store, _, err := buildProjectStore(cfg, d.Conn, repo)
	if err != nil {
		t.Fatalf("buildProjectStore should boot despite a project failing to load, got error: %v", err)
	}

	// The invariant: the stale row must still be there.
	if _, err := repo.GetProject("stale-proj"); err != nil {
		t.Errorf("stale-proj DB row must be preserved (never auto-deleted), got: %v", err)
	}
	st := store.Status("stale-proj")
	if st.State != orchestrator.StatusDegraded {
		t.Errorf("stale-proj Status.State = %q, want %q", st.State, orchestrator.StatusDegraded)
	}
	if st.Message == "" {
		t.Error("expected a non-empty degraded status message for stale-proj")
	}

	if _, ok := store.Get("healthy-proj"); !ok {
		t.Error("healthy-proj should still be loaded into the store")
	}
	if st := store.Status("healthy-proj"); st.State != orchestrator.StatusReady {
		t.Errorf("healthy-proj Status.State = %q, want %q", st.State, orchestrator.StatusReady)
	}
}
