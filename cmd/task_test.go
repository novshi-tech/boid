package cmd

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

func TestTaskCreateSpec_DependsOnPayload(t *testing.T) {
	input := `
project_id: proj-1
title: My Task
behavior: dev
depends_on_payload: task-abc
`
	var spec taskCreateSpec
	if err := yaml.Unmarshal([]byte(input), &spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spec.DependsOnPayload != "task-abc" {
		t.Errorf("DependsOnPayload = %q, want %q", spec.DependsOnPayload, "task-abc")
	}
}

func TestTaskCreateSpec_DependsOnPayload_Empty(t *testing.T) {
	input := `
project_id: proj-1
title: My Task
behavior: dev
`
	var spec taskCreateSpec
	if err := yaml.Unmarshal([]byte(input), &spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spec.DependsOnPayload != "" {
		t.Errorf("DependsOnPayload = %q, want empty string", spec.DependsOnPayload)
	}
}

func TestTaskCreateSpec_DependsOnPayload_WithDependsOn(t *testing.T) {
	input := `
project_id: proj-1
title: My Task
behavior: dev
depends_on:
  - task-x
  - task-y
depends_on_payload: task-x
`
	var spec taskCreateSpec
	if err := yaml.Unmarshal([]byte(input), &spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spec.DependsOnPayload != "task-x" {
		t.Errorf("DependsOnPayload = %q, want %q", spec.DependsOnPayload, "task-x")
	}
	if len(spec.DependsOn) != 2 {
		t.Errorf("DependsOn length = %d, want 2", len(spec.DependsOn))
	}
}

func TestTaskCreateSpec_BehaviorSpec_ParsedFromYAML(t *testing.T) {
	input := `
project_id: proj-1
title: Kit Task
behavior_spec:
  name: kit/conflict-fix
  traits:
    - instructions
  worktree: true
`
	var spec taskCreateSpec
	if err := yaml.Unmarshal([]byte(input), &spec); err != nil {
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

func TestTaskCreateSpec_BehaviorSpec_TypeAssert(t *testing.T) {
	// Verify the type is *orchestrator.BehaviorSpec (compile-time check via assignment)
	input := `
project_id: proj-1
title: Kit Task
behavior_spec:
  name: kit/test
`
	var spec taskCreateSpec
	if err := yaml.Unmarshal([]byte(input), &spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var _ *orchestrator.BehaviorSpec = spec.BehaviorSpec
}
