package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestAssignDefaultWorkspaceToUnlinked_LinksOnlyMissing(t *testing.T) {
	d := testutil.NewTestDB(t)

	// 3 projects: linked / unlinked / unlinked.
	for _, id := range []string{"already-linked", "lonely-1", "lonely-2"} {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatalf("create project %q: %v", id, err)
		}
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "already-linked", "ws-existing"); err != nil {
		t.Fatalf("link already-linked: %v", err)
	}

	n, err := orchestrator.AssignDefaultWorkspaceToUnlinked(d.Conn, orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("AssignDefaultWorkspaceToUnlinked: %v", err)
	}
	if n != 2 {
		t.Errorf("linked count = %d, want 2 (only the unlinked projects)", n)
	}

	// already-linked must keep its original workspace.
	p, err := orchestrator.GetProject(d.Conn, "already-linked")
	if err != nil {
		t.Fatalf("get already-linked: %v", err)
	}
	if p.WorkspaceID != "ws-existing" {
		t.Errorf("already-linked workspace clobbered: got %q, want %q", p.WorkspaceID, "ws-existing")
	}

	// Both lonely projects now linked to default.
	for _, id := range []string{"lonely-1", "lonely-2"} {
		p, err := orchestrator.GetProject(d.Conn, id)
		if err != nil {
			t.Fatalf("get %q: %v", id, err)
		}
		if p.WorkspaceID != orchestrator.DefaultWorkspaceSlug {
			t.Errorf("%q workspace = %q, want %q", id, p.WorkspaceID, orchestrator.DefaultWorkspaceSlug)
		}
	}
}

func TestAssignDefaultWorkspaceToUnlinked_Idempotent(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "p1", WorkDir: "/tmp/p1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// First call links one row.
	n, err := orchestrator.AssignDefaultWorkspaceToUnlinked(d.Conn, orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if n != 1 {
		t.Errorf("first call: linked = %d, want 1", n)
	}

	// Second call must be a no-op (no rows to insert).
	n, err = orchestrator.AssignDefaultWorkspaceToUnlinked(d.Conn, orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n != 0 {
		t.Errorf("second call: linked = %d, want 0 (idempotent)", n)
	}
}

func TestAssignDefaultWorkspaceToUnlinked_RejectsInvalidSlug(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "p1", WorkDir: "/tmp/p1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	for _, bad := range []string{"", "UPPER", "with space", "with_underscore"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := orchestrator.AssignDefaultWorkspaceToUnlinked(d.Conn, bad); err == nil {
				t.Errorf("expected error for slug %q, got nil", bad)
			}
		})
	}
}
