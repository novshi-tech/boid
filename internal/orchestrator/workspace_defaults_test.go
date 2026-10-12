package orchestrator

import (
	"context"
	"testing"
)

func TestWorkspaceDefaultsWithoutReceiver(t *testing.T) {
	store := NewProjectStore()
	wsStore := NewWorkspaceStore(t.TempDir())
	store.SetWorkspaceStore(wsStore)
	if err := wsStore.Save("team", &WorkspaceMeta{}); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetWorkspaceMeta(context.Background(), "team")
	if err != nil {
		t.Fatal(err)
	}
	if meta.CardEvents.Command != "judge" || !meta.CardCommands["judge"].CardWrite || meta.TaskBehaviors["judge"].DefaultInstruction == nil {
		t.Fatalf("workspace defaults missing: %+v", meta)
	}
	if len(store.metas) != 0 {
		t.Fatal("workspace resolution created a project")
	}
}
