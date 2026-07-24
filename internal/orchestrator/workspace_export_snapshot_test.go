package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestSnapshotWorkspacesForExport_AllWorkspaces(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)
	repo := NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &WorkspaceMeta{HostCommands: []string{"gh"}}); err != nil {
		t.Fatalf("Save(team-a): %v", err)
	}
	if err := repo.Save("team-b", &WorkspaceMeta{}); err != nil {
		t.Fatalf("Save(team-b): %v", err)
	}
	if err := CreateProject(d.Conn, &Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := SetProjectWorkspace(d.Conn, "proj-1", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	snaps, err := SnapshotWorkspacesForExport(d.Conn, nil)
	if err != nil {
		t.Fatalf("SnapshotWorkspacesForExport: %v", err)
	}
	bySlug := map[string]*WorkspaceExportSnapshot{}
	for _, s := range snaps {
		bySlug[s.Slug] = s
	}
	if _, ok := bySlug["team-a"]; !ok {
		t.Fatal("team-a missing from snapshot")
	}
	if _, ok := bySlug["team-b"]; !ok {
		t.Fatal("team-b missing from snapshot")
	}
	if !equalStrSlice(bySlug["team-a"].ProjectIDs, []string{"proj-1"}) {
		t.Errorf("team-a.ProjectIDs = %v, want [proj-1]", bySlug["team-a"].ProjectIDs)
	}
	if len(bySlug["team-b"].ProjectIDs) != 0 {
		t.Errorf("team-b.ProjectIDs = %v, want empty", bySlug["team-b"].ProjectIDs)
	}
}

func TestSnapshotWorkspacesForExport_SingleWorkspace(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)
	repo := NewWorkspaceRepository(d.Conn)
	if err := repo.Save("only-me", &WorkspaceMeta{HostCommands: []string{"aws"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := repo.Save("someone-else", &WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	snaps, err := SnapshotWorkspacesForExport(d.Conn, []string{"only-me"})
	if err != nil {
		t.Fatalf("SnapshotWorkspacesForExport: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Slug != "only-me" {
		t.Fatalf("snaps = %+v, want exactly [only-me]", snaps)
	}
	if !equalStrSlice(snaps[0].Meta.HostCommands, []string{"aws"}) {
		t.Errorf("HostCommands = %v, want [aws]", snaps[0].Meta.HostCommands)
	}
}

// TestSnapshotWorkspacesForExport_ProjectsMapMatchesProjectIDs pins the
// round-2 extension of Blocker 3: every assigned project's full row must
// come from the SAME transaction ProjectIDs did (WorkspaceExportSnapshot.
// Projects), so a caller (ProjectAppService.ExportWorkspaceEnvelopes) never
// needs a second, separately-fallible, post-transaction GetProject lookup.
func TestSnapshotWorkspacesForExport_ProjectsMapMatchesProjectIDs(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)
	repo := NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := CreateProject(d.Conn, &Project{ID: "proj-1", WorkDir: "/tmp/proj-1", UpstreamURL: "https://example.com/proj-1.git"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := SetProjectWorkspace(d.Conn, "proj-1", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	snaps, err := SnapshotWorkspacesForExport(d.Conn, []string{"team-a"})
	if err != nil {
		t.Fatalf("SnapshotWorkspacesForExport: %v", err)
	}
	snap := snaps[0]
	if len(snap.Projects) != 1 {
		t.Fatalf("Projects = %+v, want exactly 1 entry", snap.Projects)
	}
	p, ok := snap.Projects["proj-1"]
	if !ok || p == nil {
		t.Fatal("Projects[\"proj-1\"] missing")
	}
	if p.UpstreamURL != "https://example.com/proj-1.git" {
		t.Errorf("UpstreamURL = %q, want https://example.com/proj-1.git", p.UpstreamURL)
	}
	for _, id := range snap.ProjectIDs {
		if snap.Projects[id] == nil {
			t.Errorf("ProjectIDs contains %q but Projects has no entry for it", id)
		}
	}
}

func TestSnapshotWorkspacesForExport_UnknownSlugIsNotExist(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)

	_, err := SnapshotWorkspacesForExport(d.Conn, []string{"nonexistent"})
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want wrapped os.ErrNotExist", err)
	}
}

// TestSnapshotWorkspacesForExport_ConsistentUnderConcurrentReassignment pins
// Blocker 3: while an export snapshot is being read, concurrent
// SetProjectWorkspace reassignments (what `boid workspace assign` performs)
// must never cause a project to appear in zero or two workspaces across the
// snapshot's own results — every project_id ends up in EXACTLY one
// snapshot's ProjectIDs list, every time.
func TestSnapshotWorkspacesForExport_ConsistentUnderConcurrentReassignment(t *testing.T) {
	d := newTestApplyDB(t)
	repo := NewWorkspaceRepository(d.Conn)
	if err := repo.Save("ws-a", &WorkspaceMeta{}); err != nil {
		t.Fatalf("Save(ws-a): %v", err)
	}
	if err := repo.Save("ws-b", &WorkspaceMeta{}); err != nil {
		t.Fatalf("Save(ws-b): %v", err)
	}

	const numProjects = 20
	ids := make([]string, numProjects)
	for i := 0; i < numProjects; i++ {
		id := fmt.Sprintf("proj-%02d", i)
		ids[i] = id
		if err := CreateProject(d.Conn, &Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatalf("CreateProject(%s): %v", id, err)
		}
		if err := SetProjectWorkspace(d.Conn, id, "ws-a"); err != nil {
			t.Fatalf("SetProjectWorkspace(%s): %v", id, err)
		}
	}

	const iterations = 30
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := ids[i%numProjects]
			target := "ws-a"
			if i%2 == 0 {
				target = "ws-b"
			}
			if err := SetProjectWorkspace(d.Conn, id, target); err != nil {
				t.Errorf("SetProjectWorkspace(%s, %s): %v", id, target, err)
				return
			}
		}
	}()

	for i := 0; i < iterations; i++ {
		snaps, err := SnapshotWorkspacesForExport(d.Conn, nil)
		if err != nil {
			t.Fatalf("SnapshotWorkspacesForExport: %v", err)
		}
		seen := map[string]int{}
		for _, s := range snaps {
			for _, id := range s.ProjectIDs {
				seen[id]++
			}
		}
		for _, id := range ids {
			if seen[id] != 1 {
				t.Fatalf("project %q appeared %d times across the snapshot (want exactly 1): %+v", id, seen[id], snaps)
			}
		}
	}
	close(stop)
	wg.Wait()
}
