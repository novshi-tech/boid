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
// validation + normalization pipeline project.yaml's own task_behaviors go
// through (決定4, 論点j).

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
	// "dev" is the legacy alias of "executor" (BehaviorAliases, spec_types.go)
	// — addAliasMirrors must have run as part of the save-path normalization
	// even though this test only ever wrote "executor" by name.
	if _, ok := got.TaskBehaviors["dev"]; !ok {
		t.Errorf("TaskBehaviors missing the \"dev\" alias mirror of \"executor\" — normalizeWorkspaceDefaultTaskBehaviors did not run addAliasMirrors on save")
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

// TestWorkspaceRepository_Save_ToleratesAlreadyMirroredAliasAndCanonical
// documents the deliberate DB-save-vs-envelope-decode asymmetry
// (normalizeWorkspaceDefaultTaskBehaviors's alreadyMayBeMirrored parameter,
// spec_loader.go): Save strips alias mirrors BEFORE re-normalizing (matching
// GetWithWorkspace's own re-processing idiom), so — unlike
// decodeWorkspaceEnvelopeSpec, which is the layer that actually catches a
// GENUINELY ambiguous fresh document (see
// TestDecodeWorkspaceEnvelopeDocuments_RejectsDuplicateTaskBehaviorAlias in
// workspace_envelope_test.go) — a direct repo.Save call with both "dev" and
// "executor" present does NOT error: "dev" is treated as a (possibly stale)
// mirror of "executor" and silently dropped in favor of it. This is
// intentional: Save's job is idempotent re-normalization of a value that may
// already have been through this exact pipeline once, not first-line
// ambiguity detection.
func TestWorkspaceRepository_Save_ToleratesAlreadyMirroredAliasAndCanonical(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	meta := &WorkspaceMeta{
		TaskBehaviors: map[string]TaskBehavior{
			"dev":      {Traits: []string{"stale-mirror-value"}},
			"executor": {Traits: []string{"canonical-value"}},
		},
	}
	if err := repo.Save("ws-dup-alias", meta); err != nil {
		t.Fatalf("Save: %v (Save's normalization strips alias mirrors before re-checking, so this must not error)", err)
	}

	got, err := repo.Load("ws-dup-alias")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// "dev" was stripped as a mirror candidate (both keys were present) and
	// then addAliasMirrors re-added it from "executor"'s value — so the
	// stale "dev" content never survives, only "executor"'s does.
	if behavior := got.TaskBehaviors["dev"]; len(behavior.Traits) != 1 || behavior.Traits[0] != "canonical-value" {
		t.Errorf("TaskBehaviors[dev] = %+v, want it to mirror executor's value (canonical-value), not survive as its own stale definition", behavior)
	}
}

// TestWorkspaceRepository_SaveLoadSave_NoDuplicateAliasErrorOnResave is the
// idempotency regression this PR's normalizeWorkspaceDefaultTaskBehaviors
// design exists to prevent (論点j): a value that already went through the
// pipeline once (Save added the "dev" alias mirror of "executor") must
// tolerate going through it a SECOND time (Load → Save again, e.g. a
// round trip through `boid workspace export` → `apply`) without tripping
// the duplicate-alias-and-canonical guard on its own previously-added
// mirror entry.
func TestWorkspaceRepository_SaveLoadSave_NoDuplicateAliasErrorOnResave(t *testing.T) {
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
	if _, ok := loaded.TaskBehaviors["dev"]; !ok {
		t.Fatalf("precondition failed: Load did not come back with the \"dev\" alias mirror (got %+v)", loaded.TaskBehaviors)
	}

	if err := repo.Save("ws-resave", loaded); err != nil {
		t.Fatalf("second Save (re-saving an already-mirrored map) must not error: %v", err)
	}

	got, err := repo.Load("ws-resave")
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	if _, ok := got.TaskBehaviors["executor"]; !ok {
		t.Errorf("TaskBehaviors after resave = %+v, want an \"executor\" entry to survive", got.TaskBehaviors)
	}
	if _, ok := got.TaskBehaviors["dev"]; !ok {
		t.Errorf("TaskBehaviors after resave = %+v, want the \"dev\" alias mirror to survive", got.TaskBehaviors)
	}
}
