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

// TestResolveBehavior_PlanIsAFreeName pins the post-alias-removal contract for
// "plan": it is an ordinary project-chosen behavior name with no special
// meaning. It resolves to itself (not to "supervisor") and takes the Track A2
// fail-safe readonly default, exactly like any other free name.
func TestResolveBehavior_PlanIsAFreeName(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"plan": {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "plan"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "plan" {
		t.Errorf("BehaviorName = %q, want %q (no alias rewrite)", res.BehaviorName, "plan")
	}
	if !res.Readonly {
		t.Error("Readonly = false, want true (free name takes the fail-safe default)")
	}
}

// TestResolveBehavior_DevIsAFreeName is the "dev" counterpart. Note the
// readonly flip this removal is responsible for: while "dev" was an alias of
// "executor" it inherited executor's readonly=false compat exception; as a
// free name it now takes the fail-safe default (readonly=true) unless the
// project sets readonly explicitly.
func TestResolveBehavior_DevIsAFreeName(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	res, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: "dev"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BehaviorName != "dev" {
		t.Errorf("BehaviorName = %q, want %q (no alias rewrite)", res.BehaviorName, "dev")
	}
	if !res.Readonly {
		t.Error("Readonly = false, want true (free name takes the fail-safe default)")
	}
}

// TestResolveBehavior_LegacyAliasNoLongerResolves verifies that the alias
// fallback is gone in both directions: asking for "plan" against a project
// that only defines "supervisor" is now an unknown-behavior error rather than
// a silent redirect, and vice versa.
func TestResolveBehavior_LegacyAliasNoLongerResolves(t *testing.T) {
	cases := []struct {
		defined   string
		requested string
	}{
		{defined: "supervisor", requested: "plan"},
		{defined: "plan", requested: "supervisor"},
		{defined: "executor", requested: "dev"},
		{defined: "dev", requested: "executor"},
	}
	for _, tc := range cases {
		t.Run(tc.requested+"/"+tc.defined, func(t *testing.T) {
			meta := &orchestrator.ProjectMeta{
				TaskBehaviors: map[string]orchestrator.TaskBehavior{
					tc.defined: {},
				},
			}
			_, err := orchestrator.ResolveBehavior(meta, orchestrator.BehaviorResolveRequest{Behavior: tc.requested})
			if err == nil {
				t.Fatalf("requesting %q against a project defining only %q: expected error, got nil",
					tc.requested, tc.defined)
			}
		})
	}
}

func TestResolveBehavior_InlineBehaviorSpec(t *testing.T) {
	meta := &orchestrator.ProjectMeta{BaseBranch: "main"}
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
}

func TestLookupBehavior_ExactMatch(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {Traits: []string{"artifact"}},
		},
	}
	b, ok := orchestrator.LookupBehavior(meta, "supervisor")
	if !ok {
		t.Fatal("expected to find supervisor, got not found")
	}
	if len(b.Traits) != 1 || b.Traits[0] != "artifact" {
		t.Errorf("traits = %v, want [artifact]", b.Traits)
	}
}

func TestLookupBehavior_NotFound(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{},
	}
	if _, ok := orchestrator.LookupBehavior(meta, "supervisor"); ok {
		t.Error("expected not found, got found")
	}
}

// TestLookupBehavior_NilMeta pins the nil-safety the helper exists for: the
// alias-era version dereferenced meta unconditionally, and several callers
// (ResolveBehavior's nil-meta paths, DefaultBehaviorResolvable) rely on not
// having to guard separately.
func TestLookupBehavior_NilMeta(t *testing.T) {
	if _, ok := orchestrator.LookupBehavior(nil, "supervisor"); ok {
		t.Error("expected not found for nil meta, got found")
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
