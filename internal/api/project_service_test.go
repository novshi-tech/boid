package api

import (
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestSetProjectWorkspace_RejectsNonExistentSlug pins MAJOR 5 (codex review,
// docs/plans/workspace-db-consolidation.md): assigning a project to a
// workspace slug with no corresponding workspaces row must be rejected up
// front, rather than silently leaving a dangling project_workspaces
// reference. Before this fix, SetProjectWorkspace(dbtx, ...) accepted any
// syntactically valid slug — dispatch would then run in a permanently
// degraded window (GetWithWorkspace logs "workspace.yaml not found" every
// call) that never self-heals, since MigrateWorkspaceYAMLToDB's
// project->workspace reference recheck only ever runs once
// (state=committed skips every later daemon start).
//
// PR3 had to revert this check (e2e regression: the CLI had no way to
// create a workspaces row outside the migration path, so every
// "yaml on disk → `boid workspace assign`" flow 404'd). PR4 reinstates it
// alongside that create path (POST /api/workspaces, cmd/workspace.go's
// assign auto-create — see runWorkspaceAssign).
func TestSetProjectWorkspace_RejectsNonExistentSlug(t *testing.T) {
	repo := &stubProjectRepository{
		projects:           []*orchestrator.Project{{ID: "proj-1"}},
		existingWorkspaces: map[string]bool{},
	}
	svc := &ProjectAppService{Projects: repo, Meta: &stubProjectMetaStore{}}

	_, err := svc.SetProjectWorkspace("proj-1", "ghost")
	if err == nil {
		t.Fatal("expected error assigning a nonexistent workspace slug")
	}
	statusErr, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if statusErr.Code != http.StatusNotFound {
		t.Errorf("status code = %d, want %d", statusErr.Code, http.StatusNotFound)
	}
}

// TestSetProjectWorkspace_AcceptsDefaultSlug verifies DefaultWorkspaceSlug is
// exempt from the existence check (WorkspaceRepository.EnsureDefault
// guarantees it always exists / self-heals), even when the repository stub
// reports no workspaces at all.
func TestSetProjectWorkspace_AcceptsDefaultSlug(t *testing.T) {
	repo := &stubProjectRepository{
		projects:           []*orchestrator.Project{{ID: "proj-1"}},
		existingWorkspaces: map[string]bool{},
	}
	svc := &ProjectAppService{Projects: repo, Meta: &stubProjectMetaStore{}}

	project, err := svc.SetProjectWorkspace("proj-1", orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}
	if project.WorkspaceID != orchestrator.DefaultWorkspaceSlug {
		t.Errorf("WorkspaceID = %q, want %q", project.WorkspaceID, orchestrator.DefaultWorkspaceSlug)
	}
}

// TestSetProjectWorkspace_AcceptsExistingSlug is the regression guard
// alongside the rejection test above: a slug that does exist must still be
// accepted and assigned normally.
func TestSetProjectWorkspace_AcceptsExistingSlug(t *testing.T) {
	repo := &stubProjectRepository{
		projects:           []*orchestrator.Project{{ID: "proj-1"}},
		existingWorkspaces: map[string]bool{"team-a": true},
	}
	svc := &ProjectAppService{Projects: repo, Meta: &stubProjectMetaStore{}}

	project, err := svc.SetProjectWorkspace("proj-1", "team-a")
	if err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}
	if project.WorkspaceID != "team-a" {
		t.Errorf("WorkspaceID = %q, want team-a", project.WorkspaceID)
	}
}

// TestSetProjectWorkspace_RejectsDeletedWorkspaceMidAssign pins MAJOR 3
// (codex review, docs/plans/workspace-db-consolidation.md): assigning to a
// slug that turns out not to exist (e.g. it was deleted moments before this
// call landed) must be rejected atomically through a single
// AssignWorkspaceIfExists call — replacing the previous WorkspaceExists +
// SetProjectWorkspace two-step, which had a window between the existence
// check and the assign where a concurrent DELETE could slip in and leave a
// dangling project_workspaces reference. This test asserts the single call
// happens and that the in-memory cache is never touched when it fails, so a
// caller can never observe a project cached against a workspace that does
// not exist in the DB.
func TestSetProjectWorkspace_RejectsDeletedWorkspaceMidAssign(t *testing.T) {
	repo := &stubProjectRepository{
		projects:           []*orchestrator.Project{{ID: "proj-1"}},
		existingWorkspaces: map[string]bool{}, // team-a does not exist (e.g. deleted just before this call)
	}
	meta := newRecordingMetaStore()
	svc := &ProjectAppService{Projects: repo, Meta: meta}

	_, err := svc.SetProjectWorkspace("proj-1", "team-a")
	if err == nil {
		t.Fatal("expected error assigning to a workspace that does not exist")
	}
	statusErr, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if statusErr.Code != http.StatusNotFound {
		t.Errorf("status code = %d, want %d", statusErr.Code, http.StatusNotFound)
	}
	if len(repo.assignWorkspaceIfExistsCalls) != 1 {
		t.Errorf("expected exactly one atomic AssignWorkspaceIfExists call, got %v", repo.assignWorkspaceIfExistsCalls)
	}
	if _, called := meta.setWorkspaceIDCalls["proj-1"]; called {
		t.Error("in-memory cache must not be updated when the atomic assign is rejected (no dangling reference)")
	}
}

// TestSetProjectWorkspace_EmptyClearsWithoutExistenceCheck is a regression
// guard: clearing the assignment (workspaceID == "") must not go through the
// existence check at all (there is no slug to check), and must still
// succeed even when the repository reports no workspaces.
func TestSetProjectWorkspace_EmptyClearsWithoutExistenceCheck(t *testing.T) {
	repo := &stubProjectRepository{
		projects:           []*orchestrator.Project{{ID: "proj-1", WorkspaceID: "team-a"}},
		existingWorkspaces: map[string]bool{},
	}
	svc := &ProjectAppService{Projects: repo, Meta: &stubProjectMetaStore{}}

	project, err := svc.SetProjectWorkspace("proj-1", "")
	if err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}
	if project.WorkspaceID != "" {
		t.Errorf("WorkspaceID = %q, want empty after clear", project.WorkspaceID)
	}
}
