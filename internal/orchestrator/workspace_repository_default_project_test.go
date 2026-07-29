package orchestrator

import (
	"strings"
	"testing"
)

// This file pins PR3 of docs/plans/workspace-default-project.md: the
// workspaces table's 4 new "default project definition" columns
// (task_behaviors / base_branch / fork_point / default_task_behavior) round
// trip through WorkspaceRepository, and every write path (Create/Save via
// saveWorkspaceRow/UpdateIfRevisionMatches, all funneling through
// marshalWorkspaceMetaColumns) runs task_behaviors through the same
// validation project.yaml's own task_behaviors go through (決定4, 論点j).
//
// normalizeWorkspaceDefaultTaskBehaviors (spec_loader.go) deliberately does
// NOT add alias mirrors here — see that function's own doc comment (codex
// review on PR3, Major 1) for why: persisting a mirror entry would have
// made a workspace's own honest export unable to be re-applied.

// TestWorkspaceRepository_SaveLoad_RoundTrip_DefaultProjectFields is the
// doc's own PR3 requirement: "Save → Load → DeepEqual の round-trip テストを
// 新フィールド込みで必ず置く". Includes a Hook with Command set specifically
// because TaskBehavior.Hooks is `json:"-"` — the concrete regression this
// guards is task_behaviors silently losing its hooks if a future change
// swapped the YAML-based column encoding back to encoding/json.
func TestWorkspaceRepository_SaveLoad_RoundTrip_DefaultProjectFields(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	meta := &WorkspaceMeta{
		TaskBehaviors: map[string]TaskBehavior{
			"executor": {
				Traits: []string{"impl"},
				Hooks: []Hook{
					{ID: "main", Command: "echo hi"},
				},
			},
		},
		BaseBranch:          "main",
		ForkPoint:           "origin/main",
		DefaultTaskBehavior: "executor",
	}
	if err := repo.Save("ws-default-project", meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.Load("ws-default-project")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", got.BaseBranch)
	}
	if got.ForkPoint != "origin/main" {
		t.Errorf("ForkPoint = %q, want origin/main", got.ForkPoint)
	}
	if got.DefaultTaskBehavior != "executor" {
		t.Errorf("DefaultTaskBehavior = %q, want executor", got.DefaultTaskBehavior)
	}
	if len(got.TaskBehaviors) != 1 {
		t.Fatalf("TaskBehaviors = %+v, want exactly one entry (no alias mirror should be persisted)", got.TaskBehaviors)
	}
	behavior, ok := got.TaskBehaviors["executor"]
	if !ok {
		t.Fatalf("TaskBehaviors = %+v, want an \"executor\" entry", got.TaskBehaviors)
	}
	if len(behavior.Traits) != 1 || behavior.Traits[0] != "impl" {
		t.Errorf("executor.Traits = %v, want [impl]", behavior.Traits)
	}
	if len(behavior.Hooks) != 1 || behavior.Hooks[0].ID != "main" || behavior.Hooks[0].Command != "echo hi" {
		t.Errorf("executor.Hooks = %+v, want one hook {ID: main, Command: echo hi} — "+
			"if this is empty, task_behaviors is being marshaled with encoding/json "+
			"instead of yaml.v3 and silently dropping Hooks (json:\"-\")", behavior.Hooks)
	}
}

func TestWorkspaceRepository_Save_EmptyMetaRoundTripsToEmptyDefaultProjectFields(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Save("empty-ws-default-project", &WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.Load("empty-ws-default-project")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.TaskBehaviors) != 0 {
		t.Errorf("TaskBehaviors: got %v, want empty", got.TaskBehaviors)
	}
	if got.BaseBranch != "" {
		t.Errorf("BaseBranch: got %q, want empty", got.BaseBranch)
	}
	if got.ForkPoint != "" {
		t.Errorf("ForkPoint: got %q, want empty", got.ForkPoint)
	}
	if got.DefaultTaskBehavior != "" {
		t.Errorf("DefaultTaskBehavior: got %q, want empty", got.DefaultTaskBehavior)
	}
}

// TestWorkspaceRepository_Save_RejectsInvalidHookKind pins the "DB save"
// entry point half of 決定/論点j: a malformed hook must be rejected before
// it ever reaches the database, exactly like project.yaml's own load-time
// validateHookKind check (spec_loader.go).
func TestWorkspaceRepository_Save_RejectsInvalidHookKind(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	meta := &WorkspaceMeta{
		TaskBehaviors: map[string]TaskBehavior{
			"executor": {
				Hooks: []Hook{
					{ID: "bad", Kind: HandlerKindAgent, Command: "echo hi"}, // agent kind + command is invalid
				},
			},
		},
	}
	err := repo.Save("ws-bad-hook", meta)
	if err == nil {
		t.Fatal("expected Save to reject an invalid hook kind, got nil error")
	}
	if !strings.Contains(err.Error(), "does not allow 'command:'") {
		t.Errorf("error = %v, want it to mention the agent/command conflict", err)
	}
}

// TestWorkspaceRepository_Save_RejectsDuplicateAliasAndCanonical pins the
// other normalizeBehaviorAliases guard: a workspace default that defines
// BOTH a legacy alias ("dev") and its canonical name ("executor") is
// ambiguous and must be rejected at save time, same as project.yaml and
// same as decodeWorkspaceEnvelopeSpec
// (TestDecodeWorkspaceEnvelopeDocuments_RejectsDuplicateTaskBehaviorAlias,
// workspace_envelope_default_project_test.go). Since
// normalizeWorkspaceDefaultTaskBehaviors never strips alias mirrors (Major 1
// fix, spec_loader.go), Save provides this same first-line protection for
// any caller that constructs a WorkspaceMeta directly rather than via
// envelope apply.
func TestWorkspaceRepository_Save_RejectsDuplicateAliasAndCanonical(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	meta := &WorkspaceMeta{
		TaskBehaviors: map[string]TaskBehavior{
			"dev":      {},
			"executor": {},
		},
	}
	err := repo.Save("ws-dup-alias", meta)
	if err == nil {
		t.Fatal("expected Save to reject a task_behaviors map with both an alias and its canonical name, got nil error")
	}
	if !strings.Contains(err.Error(), "duplicate task behavior definition") {
		t.Errorf("error = %v, want it to mention the duplicate definition", err)
	}
}

// TestWorkspaceRepository_SaveLoadSave_Idempotent is the idempotency
// regression normalizeWorkspaceDefaultTaskBehaviors's no-mirroring design
// exists to guarantee: a value read back from Load (which never carries a
// mirror, since Save never persisted one) must save again cleanly — the
// ordinary "load it, tweak something unrelated, save it back" cycle every
// UpdateWorkspace/apply call performs.
func TestWorkspaceRepository_SaveLoadSave_Idempotent(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	meta := &WorkspaceMeta{
		TaskBehaviors: map[string]TaskBehavior{
			"executor": {Traits: []string{"impl"}},
		},
	}
	if err := repo.Save("ws-resave", meta); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	loaded, err := repo.Load("ws-resave")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := repo.Save("ws-resave", loaded); err != nil {
		t.Fatalf("second Save (re-saving a freshly-Loaded value) must not error: %v", err)
	}

	got, err := repo.Load("ws-resave")
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	if len(got.TaskBehaviors) != 1 {
		t.Fatalf("TaskBehaviors after resave = %+v, want exactly the one \"executor\" entry (no mirror should ever appear)", got.TaskBehaviors)
	}
	if _, ok := got.TaskBehaviors["executor"]; !ok {
		t.Errorf("TaskBehaviors after resave = %+v, want an \"executor\" entry to survive", got.TaskBehaviors)
	}
}
