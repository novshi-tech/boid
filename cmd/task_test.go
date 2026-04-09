package cmd

import (
	"testing"

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
