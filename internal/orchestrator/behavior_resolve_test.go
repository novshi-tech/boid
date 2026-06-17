package orchestrator_test

import (
	"strings"
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

// Track A2: free naming and readonly defaults.

// TestResolveBehavior_FreeNaming_ReadonlyDefaultTrue verifies that non-canonical
// behaviors default to readonly=true (fail-safe).
func TestResolveBehavior_FreeNaming_ReadonlyDefaultTrue(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"my-research": {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "my-research"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "my-research" {
		t.Errorf("BehaviorName = %q, want %q", res.BehaviorName, "my-research")
	}
	if !res.Readonly {
		t.Error("Readonly = false, want true (non-canonical defaults to fail-safe readonly=true)")
	}
}

// TestResolveBehavior_BehaviorExplicitReadonly_False verifies that a behavior
// with explicit readonly:false in YAML gets readonly=false.
func TestResolveBehavior_BehaviorExplicitReadonly_False(t *testing.T) {
	falseVal := false
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev-task": {Readonly: &falseVal},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "dev-task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Readonly {
		t.Error("Readonly = true, want false (explicit readonly:false in behavior)")
	}
}

// TestResolveBehavior_BehaviorExplicitReadonly_True verifies that a behavior
// with explicit readonly:true in YAML gets readonly=true.
func TestResolveBehavior_BehaviorExplicitReadonly_True(t *testing.T) {
	trueVal := true
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev-task": {Readonly: &trueVal},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "dev-task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Readonly {
		t.Error("Readonly = false, want true (explicit readonly:true in behavior)")
	}
}

// TestResolveBehavior_ExecutorExplicitReadonlyTrue verifies that canonical
// "executor" with explicit readonly:true overrides the compat default.
func TestResolveBehavior_ExecutorExplicitReadonlyTrue(t *testing.T) {
	trueVal := true
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {Readonly: &trueVal},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "executor"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Readonly {
		t.Error("Readonly = false, want true (explicit readonly:true overrides executor compat)")
	}
}

// TestResolveBehavior_DefaultTaskBehavior_Used verifies that meta.DefaultTaskBehavior
// is used when behavior is omitted from the request.
func TestResolveBehavior_DefaultTaskBehavior_Used(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		DefaultTaskBehavior: "dev-task",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
			"dev-task":   {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "dev-task" {
		t.Errorf("BehaviorName = %q, want %q (should use DefaultTaskBehavior)", res.BehaviorName, "dev-task")
	}
}

// TestResolveBehavior_DefaultTaskBehavior_SkipsSupervisorFallback verifies that
// when DefaultTaskBehavior is set, the implicit supervisor fallback is NOT used,
// even if supervisor exists.
func TestResolveBehavior_DefaultTaskBehavior_SkipsSupervisorFallback(t *testing.T) {
	buf := captureSlog(t)
	meta := &orchestrator.ProjectMeta{
		DefaultTaskBehavior: "dev-task",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
			"dev-task":   {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "dev-task" {
		t.Errorf("BehaviorName = %q, want dev-task", res.BehaviorName)
	}
	if strings.Contains(buf.String(), "falling back") {
		t.Errorf("unexpected implicit-fallback warning when DefaultTaskBehavior is set: %s", buf.String())
	}
}

// TestResolveBehavior_ImplicitSupervisorFallback_EmitsWarn verifies that when
// behavior is omitted and DefaultTaskBehavior is not set, the implicit supervisor
// fallback is used and a warning is emitted.
func TestResolveBehavior_ImplicitSupervisorFallback_EmitsWarn(t *testing.T) {
	buf := captureSlog(t)
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
		t.Errorf("BehaviorName = %q, want supervisor", res.BehaviorName)
	}
	if !strings.Contains(buf.String(), "default_task_behavior") {
		t.Errorf("expected warning about missing default_task_behavior, got:\n%s", buf.String())
	}
}

// TestResolveBehavior_NoDefaultNoSupervisor_Error verifies that an error is
// returned when behavior is omitted and neither DefaultTaskBehavior nor
// a "supervisor" behavior is present in meta.
func TestResolveBehavior_NoDefaultNoSupervisor_Error(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		ID: "proj-no-default",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	_, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{})
	if err == nil {
		t.Fatal("expected error when no default_task_behavior and no supervisor, got nil")
	}
	if !strings.Contains(err.Error(), "no default_task_behavior") {
		t.Errorf("error should mention default_task_behavior, got: %v", err)
	}
}

// TestResolveBehavior_NilMeta_FallsBackToHardcodedDefault verifies that with
// nil meta, the hardcoded DefaultBehavior is used without error.
func TestResolveBehavior_NilMeta_FallsBackToHardcodedDefault(t *testing.T) {
	res, err := orchestrator.ResolveBehavior(nil, orchestrator.BehaviorResolveRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != orchestrator.DefaultBehavior {
		t.Errorf("BehaviorName = %q, want %q", res.BehaviorName, orchestrator.DefaultBehavior)
	}
}
