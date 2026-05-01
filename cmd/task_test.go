package cmd

import (
	"testing"
)

func TestParseTaskCreateSpec_DependsOnPayload(t *testing.T) {
	input := `
project_id: proj-1
title: My Task
behavior: dev
depends_on_payload: task-abc
`
	spec, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.DependsOnPayload != "task-abc" {
		t.Errorf("DependsOnPayload = %q, want %q", spec.DependsOnPayload, "task-abc")
	}
}

func TestParseTaskCreateSpec_DependsOnPayload_Empty(t *testing.T) {
	input := `
project_id: proj-1
title: My Task
behavior: dev
`
	spec, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.DependsOnPayload != "" {
		t.Errorf("DependsOnPayload = %q, want empty string", spec.DependsOnPayload)
	}
}

func TestParseTaskCreateSpec_DependsOnPayload_WithDependsOn(t *testing.T) {
	input := `
project_id: proj-1
title: My Task
behavior: dev
depends_on:
  - task-x
  - task-y
depends_on_payload: task-x
`
	spec, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.DependsOnPayload != "task-x" {
		t.Errorf("DependsOnPayload = %q, want %q", spec.DependsOnPayload, "task-x")
	}
	if len(spec.DependsOn) != 2 {
		t.Errorf("DependsOn length = %d, want 2", len(spec.DependsOn))
	}
}

func TestParseTaskCreateSpec_BehaviorSpec_ParsedFromYAML(t *testing.T) {
	input := `
project_id: proj-1
title: Kit Task
behavior_spec:
  name: kit/conflict-fix
  traits:
    - instructions
  worktree: true
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
	if !spec.BehaviorSpec.Worktree {
		t.Error("Worktree = false, want true")
	}
}

func TestParseTaskCreateSpec_AllTopLevelFields(t *testing.T) {
	// CreateTaskRequest 全フィールドが YAML で受け取れることを確認する
	// (旧 taskCreateSpec で欠落していた traits / readonly / worktree / branch_prefix /
	//  remote_id / datasource_id を含む)。
	input := `
project_id: proj-1
title: Full Task
description: a task with every field
behavior: dev
remote_id: REM-1
datasource_id: ds-github
traits:
  - artifact
readonly: true
worktree: true
branch_prefix: feat/
base_branch: main
auto_start: true
depends_on:
  - task-a
depends_on_payload: artifact.auto-merge.merged
ref: my-task
parent_id: parent-1
payload:
  foo: bar
instructions:
  - type: execution
    consumer: claude-code
    message: do the thing
`
	spec, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Traits == nil || spec.Traits[0] != "artifact" {
		t.Errorf("Traits = %v, want [artifact]", spec.Traits)
	}
	if spec.Readonly == nil || !*spec.Readonly {
		t.Errorf("Readonly = %v, want true", spec.Readonly)
	}
	if spec.Worktree == nil || !*spec.Worktree {
		t.Errorf("Worktree = %v, want true", spec.Worktree)
	}
	if spec.BranchPrefix == nil || *spec.BranchPrefix != "feat/" {
		t.Errorf("BranchPrefix = %v, want feat/", spec.BranchPrefix)
	}
	if spec.BaseBranch == nil || *spec.BaseBranch != "main" {
		t.Errorf("BaseBranch = %v, want main", spec.BaseBranch)
	}
	if spec.RemoteID != "REM-1" {
		t.Errorf("RemoteID = %q, want REM-1", spec.RemoteID)
	}
	if spec.DataSourceID != "ds-github" {
		t.Errorf("DataSourceID = %q, want ds-github", spec.DataSourceID)
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
