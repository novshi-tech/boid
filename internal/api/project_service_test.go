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
