package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestResolveBehavior_DefaultsToSupervisor(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "supervisor" {
		t.Errorf("BehaviorName = %q, want %q", res.BehaviorName, "supervisor")
	}
	if !res.Readonly {
		t.Error("Readonly = false, want true (supervisor is canonically readonly)")
	}
}

func TestResolveBehavior_AliasMapping_Plan(t *testing.T) {
	// "plan" is a legacy alias for "supervisor"
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "plan"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "supervisor" {
		t.Errorf("BehaviorName = %q, want canonical %q", res.BehaviorName, "supervisor")
	}
	if !res.Readonly {
		t.Error("Readonly = false, want true")
	}
}

func TestResolveBehavior_AliasMapping_Dev(t *testing.T) {
	// "dev" is a legacy alias for "executor"
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "dev"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "executor" {
		t.Errorf("BehaviorName = %q, want canonical %q", res.BehaviorName, "executor")
	}
	if res.Readonly {
		t.Error("Readonly = true, want false (executor is not readonly)")
	}
}

func TestResolveBehavior_InlineBehaviorSpec(t *testing.T) {
	meta := &orchestrator.ProjectMeta{Worktree: true, BaseBranch: "main"}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{
		BehaviorSpec: &orchestrator.BehaviorSpec{Name: "custom"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "custom" {
		t.Errorf("BehaviorName = %q, want %q", res.BehaviorName, "custom")
	}
}

func TestResolveBehavior_MutuallyExclusive(t *testing.T) {
	_, err := orchestrator.ResolveBehavior(nil, orchestrator.BehaviorResolveRequest{
		Behavior:     "supervisor",
		BehaviorSpec: &orchestrator.BehaviorSpec{Name: "custom"},
	})
	if err == nil {
		t.Fatal("expected error for mutually exclusive behavior+behavior_spec, got nil")
	}
}

func TestResolveBehavior_BehaviorSpec_NameRequired(t *testing.T) {
	_, err := orchestrator.ResolveBehavior(nil, orchestrator.BehaviorResolveRequest{
		BehaviorSpec: &orchestrator.BehaviorSpec{},
	})
	if err == nil {
		t.Fatal("expected error for behavior_spec with empty name, got nil")
	}
}

func TestResolveBehavior_UnknownBehavior(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	_, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "unknown"})
	if err == nil {
		t.Fatal("expected error for unknown behavior, got nil")
	}
}

func TestResolveBehavior_CanonicalOverrides_Supervisor_Readonly(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Worktree: true,
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "supervisor"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Readonly {
		t.Error("supervisor: Readonly = false, want true")
	}
}

func TestResolveBehavior_CanonicalOverrides_Executor_NotReadonly(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Worktree: true,
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "executor"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Readonly {
		t.Error("executor: Readonly = true, want false")
	}
	if !res.Worktree {
		t.Error("executor: Worktree = false, want true (from project-top)")
	}
}

func TestLookupBehaviorWithAlias_ExactMatch(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {Traits: []string{"artifact"}},
		},
	}
	b, key, ok := orchestrator.LookupBehaviorWithAlias(meta, "supervisor")
	if !ok {
		t.Fatal("expected to find supervisor, got not found")
	}
	if key != "supervisor" {
		t.Errorf("key = %q, want %q", key, "supervisor")
	}
	if len(b.Traits) != 1 || b.Traits[0] != "artifact" {
		t.Errorf("traits = %v, want [artifact]", b.Traits)
	}
}

func TestLookupBehaviorWithAlias_NotFound(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{},
	}
	_, _, ok := orchestrator.LookupBehaviorWithAlias(meta, "supervisor")
	if ok {
		t.Error("expected not found, got found")
	}
}
