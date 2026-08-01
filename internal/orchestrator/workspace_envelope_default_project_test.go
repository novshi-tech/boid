package orchestrator

import (
	"strings"
	"testing"
)

// This file pins PR3 of docs/plans/workspace-default-project.md at the
// envelope layer: task_behaviors / base_branch / fork_point /
// default_task_behavior decode, validate, normalize, merge, and export
// correctly. See workspace_repository_default_project_test.go for the
// DB-layer counterpart.

func TestDecodeWorkspaceEnvelopeDocuments_DefaultProjectFieldsPresent(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  task_behaviors:
    executor:
      traits: [impl]
      hooks:
        - id: main
          command: echo hi
  base_branch: main
  fork_point: origin/main
  default_task_behavior: executor
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	doc := docs[0]
	for _, field := range []string{"task_behaviors", "base_branch", "fork_point", "default_task_behavior"} {
		if !doc.FieldsPresent[field] {
			t.Errorf("FieldsPresent[%q] = false, want true", field)
		}
	}
	if doc.Envelope.Spec.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", doc.Envelope.Spec.BaseBranch)
	}
	if doc.Envelope.Spec.ForkPoint != "origin/main" {
		t.Errorf("ForkPoint = %q, want origin/main", doc.Envelope.Spec.ForkPoint)
	}
	if doc.Envelope.Spec.DefaultTaskBehavior != "executor" {
		t.Errorf("DefaultTaskBehavior = %q, want executor", doc.Envelope.Spec.DefaultTaskBehavior)
	}
	// Exactly one entry: normalizeWorkspaceDefaultTaskBehaviors deliberately
	// does not add an alias mirror here (codex review on PR3, Major 1) — see
	// its own doc comment (spec_loader.go) for why persisting/exporting a
	// mirror entry breaks export→apply round trips.
	if len(doc.Envelope.Spec.TaskBehaviors) != 1 {
		t.Fatalf("TaskBehaviors = %+v, want exactly one entry (no alias mirror)", doc.Envelope.Spec.TaskBehaviors)
	}
	behavior, ok := doc.Envelope.Spec.TaskBehaviors["executor"]
	if !ok {
		t.Fatalf("TaskBehaviors = %+v, want an executor entry", doc.Envelope.Spec.TaskBehaviors)
	}
	if len(behavior.Hooks) != 1 || behavior.Hooks[0].Command != "echo hi" {
		t.Errorf("executor.Hooks = %+v, want one hook with command \"echo hi\"", behavior.Hooks)
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_MissingDefaultProjectFieldsAreNotPresent(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  host_commands: [gh]
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	doc := docs[0]
	for _, field := range []string{"task_behaviors", "base_branch", "fork_point", "default_task_behavior"} {
		if doc.FieldsPresent[field] {
			t.Errorf("FieldsPresent[%q] = true, want false (key absent from source)", field)
		}
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_FormerAliasPairIsAccepted pins the
// decode-side effect of removing the alias table: a document defining both
// "dev" and "executor" (or "plan" and "supervisor") is four ordinary names,
// not two ambiguous pairs, and decodes into four distinct behaviors. This
// exact document used to be rejected as a duplicate definition, which is what
// made "plan" unusable as a behavior name for any workspace that also had a
// "supervisor".
func TestDecodeWorkspaceEnvelopeDocuments_FormerAliasPairIsAccepted(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  task_behaviors:
    dev: {}
    executor: {}
    plan: {}
    supervisor: {}
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("len(docs) = %d, want 1", len(docs))
	}
	got := docs[0].Envelope.Spec.TaskBehaviors
	if len(got) != 4 {
		t.Fatalf("TaskBehaviors = %+v, want 4 distinct entries", got)
	}
	for _, name := range []string{"dev", "executor", "plan", "supervisor"} {
		if _, ok := got[name]; !ok {
			t.Errorf("TaskBehaviors missing %q: %+v", name, got)
		}
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_RejectsInvalidHookKindInTaskBehaviors
// pins the envelope-decode half of 決定4/論点j's validation requirement:
// a malformed hook shape is rejected exactly like project.yaml's own
// load-time validateHookKind check.
func TestDecodeWorkspaceEnvelopeDocuments_RejectsInvalidHookKindInTaskBehaviors(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  task_behaviors:
    executor:
      hooks:
        - id: bad
          kind: agent
          command: echo hi
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error for an agent-kind hook with a command:, got nil")
	}
	if !strings.Contains(err.Error(), "does not allow 'command:'") {
		t.Errorf("error = %v, want it to mention the agent/command conflict", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownTaskBehaviorField pins
// the decodeStrictNode KnownFields treatment task_behaviors gets, same as
// capabilities/projects (PR-1d codex round-2 Major): a typo'd nested key
// must be rejected, not silently dropped.
func TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownTaskBehaviorField(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  task_behaviors:
    executor:
      raedonly: true
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error for a typo'd task_behaviors.executor field, got nil")
	}
}

func TestMergeInto_DefaultProjectFields_MissingPreservesCurrent(t *testing.T) {
	current := &WorkspaceMeta{
		TaskBehaviors:       map[string]TaskBehavior{"executor": {Traits: []string{"impl"}}},
		BaseBranch:          "main",
		ForkPoint:           "origin/main",
		DefaultTaskBehavior: "executor",
	}
	doc := &WorkspaceEnvelopeApply{
		Envelope:      &WorkspaceEnvelope{Spec: WorkspaceEnvelopeSpec{HostCommands: []string{"gh"}}},
		FieldsPresent: map[string]bool{"host_commands": true},
	}
	merged := doc.MergeInto(current)
	if len(merged.TaskBehaviors) != 1 || merged.TaskBehaviors["executor"].Traits[0] != "impl" {
		t.Errorf("TaskBehaviors = %+v, want preserved", merged.TaskBehaviors)
	}
	if merged.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want preserved main", merged.BaseBranch)
	}
	if merged.ForkPoint != "origin/main" {
		t.Errorf("ForkPoint = %q, want preserved origin/main", merged.ForkPoint)
	}
	if merged.DefaultTaskBehavior != "executor" {
		t.Errorf("DefaultTaskBehavior = %q, want preserved executor", merged.DefaultTaskBehavior)
	}
}

func TestMergeInto_DefaultProjectFields_PresentClearsOrReplaces(t *testing.T) {
	current := &WorkspaceMeta{
		TaskBehaviors:       map[string]TaskBehavior{"executor": {Traits: []string{"impl"}}},
		BaseBranch:          "main",
		ForkPoint:           "origin/main",
		DefaultTaskBehavior: "executor",
	}
	doc := &WorkspaceEnvelopeApply{
		Envelope: &WorkspaceEnvelope{Spec: WorkspaceEnvelopeSpec{
			TaskBehaviors:       map[string]TaskBehavior{},
			BaseBranch:          "",
			ForkPoint:           "",
			DefaultTaskBehavior: "",
		}},
		FieldsPresent: map[string]bool{
			"task_behaviors":        true,
			"base_branch":           true,
			"fork_point":            true,
			"default_task_behavior": true,
		},
	}
	merged := doc.MergeInto(current)
	if len(merged.TaskBehaviors) != 0 {
		t.Errorf("TaskBehaviors = %+v, want cleared", merged.TaskBehaviors)
	}
	if merged.BaseBranch != "" {
		t.Errorf("BaseBranch = %q, want cleared", merged.BaseBranch)
	}
	if merged.ForkPoint != "" {
		t.Errorf("ForkPoint = %q, want cleared", merged.ForkPoint)
	}
	if merged.DefaultTaskBehavior != "" {
		t.Errorf("DefaultTaskBehavior = %q, want cleared", merged.DefaultTaskBehavior)
	}
}

// TestSaveExportThenApply_TaskBehaviorsWithKnownAliasSurvives is the
// concrete end-to-end regression codex review caught on PR3 (Major 1): a
// workspace default whose only task_behaviors entry has a legacy alias
// (here, "dev" aliases to "executor" — BehaviorAliases, spec_types.go) must
// survive the REAL round trip `boid workspace export` followed by `boid
// workspace apply -f` performs: Save it to the repository, Load it back
// (this is where the bug actually lived — see below), export via
// NewWorkspaceEnvelopeFromMeta/MarshalWorkspaceEnvelope, then re-decode via
// DecodeWorkspaceEnvelopeDocuments, all without error.
//
// Going through Save/Load (rather than exporting a hand-built *WorkspaceMeta
// directly) matters: codex review's first re-verification pass flagged that
// an earlier version of this test skipped the repository round trip
// entirely, which meant it could not actually have caught the original bug
// — NewWorkspaceEnvelopeFromMeta itself never called
// normalizeWorkspaceDefaultTaskBehaviors either before or after the fix; the
// mirror only ever got introduced by marshalWorkspaceMetaColumns
// (workspace_repository.go) at Save time. Before the fix, saving
// {"executor": ...} persisted {"executor": ..., "dev": ...} (the added
// mirror); Load then read that mirrored pair straight back; exporting IT
// produced a document with both keys; and re-applying THAT export tripped
// normalizeBehaviorAliases's own duplicate-definition guard on its own
// mirror — a workspace's own honest export could never be re-applied.
func TestSaveExportThenApply_TaskBehaviorsWithKnownAliasSurvives(t *testing.T) {
	repo := newTestWorkspaceRepo(t)
	meta := &WorkspaceMeta{
		TaskBehaviors: map[string]TaskBehavior{
			"executor": {Traits: []string{"impl"}},
		},
	}
	if err := repo.Save("ws-export-apply", meta); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := repo.Load("ws-export-apply")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	envelope := NewWorkspaceEnvelopeFromMeta("team-a", loaded, nil, "")

	exported, err := MarshalWorkspaceEnvelope(envelope)
	if err != nil {
		t.Fatalf("MarshalWorkspaceEnvelope: %v", err)
	}

	docs, err := DecodeWorkspaceEnvelopeDocuments(exported)
	if err != nil {
		t.Fatalf("re-applying the export must not error, got: %v\ndocument:\n%s", err, exported)
	}
	got := docs[0].Envelope.Spec.TaskBehaviors
	if len(got) != 1 {
		t.Fatalf("TaskBehaviors after re-apply = %+v, want exactly the one \"executor\" entry", got)
	}
	if _, ok := got["executor"]; !ok {
		t.Errorf("TaskBehaviors after re-apply = %+v, want an \"executor\" entry", got)
	}
}

func TestNewWorkspaceEnvelopeFromMeta_DefaultProjectFieldsRoundTrip(t *testing.T) {
	meta := &WorkspaceMeta{
		TaskBehaviors:       map[string]TaskBehavior{"executor": {Traits: []string{"impl"}}},
		BaseBranch:          "main",
		ForkPoint:           "origin/main",
		DefaultTaskBehavior: "executor",
	}
	envelope := NewWorkspaceEnvelopeFromMeta("default", meta, nil, "")
	if envelope.Spec.BaseBranch != "main" {
		t.Errorf("Spec.BaseBranch = %q, want main", envelope.Spec.BaseBranch)
	}
	if envelope.Spec.ForkPoint != "origin/main" {
		t.Errorf("Spec.ForkPoint = %q, want origin/main", envelope.Spec.ForkPoint)
	}
	if envelope.Spec.DefaultTaskBehavior != "executor" {
		t.Errorf("Spec.DefaultTaskBehavior = %q, want executor", envelope.Spec.DefaultTaskBehavior)
	}
	if len(envelope.Spec.TaskBehaviors) != 1 || envelope.Spec.TaskBehaviors["executor"].Traits[0] != "impl" {
		t.Errorf("Spec.TaskBehaviors = %+v", envelope.Spec.TaskBehaviors)
	}
}
