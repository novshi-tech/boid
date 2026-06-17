package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseTaskCreateSpec_BehaviorSpec_ParsedFromYAML(t *testing.T) {
	// Phase 3-1: BehaviorSpec.worktree / readonly / base_branch /
	// branch_prefix / default_payload have been removed. Inline
	// behavior_spec now carries only the discriminating fields
	// (name / traits / default_instruction); worktree is taken from the
	// project-top setting at task creation time.
	input := `
project_id: proj-1
title: Kit Task
behavior_spec:
  name: kit/conflict-fix
  traits:
    - instructions
`
	spec, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Behavior != "" {
		t.Errorf("Behavior = %q, want empty", spec.Behavior)
	}
	if spec.BehaviorSpec == nil {
		t.Fatal("BehaviorSpec is nil, want non-nil")
	}
	if spec.BehaviorSpec.Name != "kit/conflict-fix" {
		t.Errorf("Name = %q, want %q", spec.BehaviorSpec.Name, "kit/conflict-fix")
	}
	if len(spec.BehaviorSpec.Traits) != 1 || spec.BehaviorSpec.Traits[0] != "instructions" {
		t.Errorf("Traits = %v, want [instructions]", spec.BehaviorSpec.Traits)
	}
}

func TestParseTaskCreateSpec_AllTopLevelFields(t *testing.T) {
	// CreateTaskRequest の全フィールドが YAML で受け取れることを確認する。
	// Phase 2-3 で worktree / branch_prefix / base_branch の task-row override
	// は廃止された。readonly は Track A1.1 で復活し first-class field になった。
	input := `
project_id: proj-1
title: Full Task
description: a task with every field
behavior: dev
remote_id: REM-1
traits:
  - artifact
auto_start: true
ref: my-task
parent_id: parent-1
payload:
  foo: bar
instructions:
  - type: execution
    agent: claude-code
    message: do the thing
`
	spec, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Traits == nil || spec.Traits[0] != "artifact" {
		t.Errorf("Traits = %v, want [artifact]", spec.Traits)
	}
	if spec.RemoteID != "REM-1" {
		t.Errorf("RemoteID = %q, want REM-1", spec.RemoteID)
	}
	if !spec.AutoStart {
		t.Error("AutoStart = false, want true")
	}
	if spec.Ref != "my-task" {
		t.Errorf("Ref = %q, want my-task", spec.Ref)
	}
	if spec.ParentID != "parent-1" {
		t.Errorf("ParentID = %q, want parent-1", spec.ParentID)
	}
	if len(spec.Payload) == 0 {
		t.Error("Payload is empty, want non-empty JSON")
	}
	if len(spec.Instructions) == 0 {
		t.Error("Instructions is empty, want non-empty JSON")
	}
}

func TestParseTaskCreateSpec_DroppedTaskRowOverrideFields(t *testing.T) {
	// Phase 2-3: worktree / branch_prefix / base_branch in a task YAML spec
	// are no longer fields on CreateTaskRequest. They are silently dropped at
	// parse time (the API server emits a slog.Warn on the wire). This test
	// pins that behavior so a future regression that re-adds a field on
	// CreateTaskRequest gets caught.
	//
	// Track A1.1: readonly is now a first-class field on CreateTaskRequest;
	// it is accepted and round-trips through the JSON encoding.
	input := `
project_id: proj-1
title: Override Task
behavior: dev
readonly: true
worktree: true
branch_prefix: feat/
base_branch: develop
`
	spec, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// readonly must survive the round-trip as a first-class field.
	if spec.Readonly == nil || !*spec.Readonly {
		t.Errorf("Readonly = %v, want *true (readonly is now a first-class field)", spec.Readonly)
	}
	// Encode the spec back to JSON and confirm the still-deprecated keys do
	// not survive.
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	for _, key := range []string{`"worktree"`, `"branch_prefix"`, `"base_branch"`} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("spec retained dropped key %s in JSON: %s", key, encoded)
		}
	}
}

func TestParseTaskCreateSpec_OmitBehavior(t *testing.T) {
	// behavior / behavior_spec を共に省略しても parse は成功する
	// (server 側で DefaultBehavior に routing される前提)。
	input := `
project_id: proj-1
title: Triage Me
description: figure out what to do
`
	spec, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Behavior != "" {
		t.Errorf("Behavior = %q, want empty", spec.Behavior)
	}
	if spec.BehaviorSpec != nil {
		t.Errorf("BehaviorSpec = %v, want nil", spec.BehaviorSpec)
	}
}

// Phase 3-1: behavior_spec.default_payload was removed; the
// orchestrator.RawPayload type lingers (used elsewhere by hooks) but
// the unmarshal contract is no longer exercised through BehaviorSpec.

func TestParseTaskCreateSpec_RejectsUnknownField(t *testing.T) {
	// typo した field 名は弾かれる。
	input := `
project_id: proj-1
title: Typo
behaviorrr: dev
`
	if _, err := parseTaskCreateSpec([]byte(input)); err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}
