package orchestrator_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestGetWithWorkspace_CardCommandsOrder_ClonedNotAliased pins that
// cloneProjectMeta's CardCommandsOrder copy (spec_loader.go) is real, for a
// project linked to a workspace (the branch that goes through
// cloneProjectMeta at all — an unlinked project's GetWithWorkspace returns
// the cached meta unchanged by design, so this test doesn't cover that
// case). A caller that mutates the slice handed back must never corrupt the
// store's own cached ProjectMeta.
func TestGetWithWorkspace_CardCommandsOrder_ClonedNotAliased(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, `id: proj-cmd-order
name: Command Order Project
card_commands:
  discuss:
    label: Discuss
    run: python3 scripts/discuss.py
  review:
    label: Run
    run: python3 scripts/review.py
`)

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "cmd-order-ws", "")

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-cmd-order", WorkDir: projectDir, WorkspaceID: "cmd-order-ws"},
	})

	want := []string{"discuss", "review"}

	first, err := s.GetWithWorkspace(context.Background(), "proj-cmd-order")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if !reflect.DeepEqual(first.CardCommandsOrder, want) {
		t.Fatalf("CardCommandsOrder = %v, want %v", first.CardCommandsOrder, want)
	}

	// Mutate the slice handed back. If cloneProjectMeta ever stops copying
	// CardCommandsOrder, this aliases the store's cached slice directly and
	// the second call below observes the corruption.
	first.CardCommandsOrder[0] = "mutated"

	second, err := s.GetWithWorkspace(context.Background(), "proj-cmd-order")
	if err != nil {
		t.Fatalf("GetWithWorkspace (second): %v", err)
	}
	if !reflect.DeepEqual(second.CardCommandsOrder, want) {
		t.Errorf("CardCommandsOrder after mutating a prior result = %v, want %v (clone not aliased)", second.CardCommandsOrder, want)
	}
}
