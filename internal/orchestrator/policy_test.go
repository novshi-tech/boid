package orchestrator

import (
	"slices"
	"testing"
)

func TestDefaultBuiltinPolicies_HookBoidOnly(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if _, ok := policies["boid"]; !ok {
		t.Error("missing boid policy")
	}
}

func TestDefaultBuiltinPolicies_HookBoidOps(t *testing.T) {
	boidP := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})["boid"]
	wantOps := []string{
		OpBoidJobDone,
		OpBoidJobList,
		OpBoidJobShow,
		OpBoidJobLog,
		OpBoidActionSend,
		OpBoidAgentStop,
		OpBoidTaskCreate,
		OpBoidTaskGet,
		OpBoidTaskUpdate,
		OpBoidTaskImport,
		OpBoidTaskReopen,
		OpBoidTaskList,
		OpBoidTaskNotify,
		OpBoidTaskAnswer,
		OpBoidTaskAsk,
		OpBoidTaskDelete,
	}
	if !opsEqual(boidP.AllowedOps, wantOps) {
		t.Errorf("hook×boid AllowedOps = %v, want %v", boidP.AllowedOps, wantOps)
	}
}

// hook×boid policy は cwd に /tmp, ProjectDir, HomeDir を許可する。
func TestDefaultBuiltinPolicies_HookBoidCwdRoots(t *testing.T) {
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: "/home/user"}
	boidPolicy := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, pctx)["boid"]
	for _, cwd := range []string{"/tmp", "/tmp/sub", "/work/project", "/work/project/sub", "/home/user", "/home/user/.boid/output"} {
		if !boidPolicy.AllowsCwd(cwd) {
			t.Errorf("hook×boid should allow cwd %q, AllowedCwdRoots=%v", cwd, boidPolicy.AllowedCwdRoots)
		}
	}
	if boidPolicy.AllowsCwd("/etc") {
		t.Errorf("hook×boid should reject cwd /etc")
	}
}

func TestDefaultBuiltinPolicies_FetchPolicy(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"fetch"}, PolicyContext{})
	p, ok := policies["fetch"]
	if !ok {
		t.Fatal("missing fetch policy")
	}
	if !p.Allows(OpFetchGet) {
		t.Errorf("fetch policy should allow op %q", OpFetchGet)
	}
}

// hook×fetch policy is identical regardless of role (no special gate overrides).
func TestDefaultBuiltinPolicies_FetchRoleInvariant(t *testing.T) {
	pHook := DefaultBuiltinPolicies(RoleHook, []string{"fetch"}, PolicyContext{})["fetch"]
	pEmpty := DefaultBuiltinPolicies("", []string{"fetch"}, PolicyContext{})["fetch"]
	if !opsEqual(pHook.AllowedOps, pEmpty.AllowedOps) {
		t.Errorf("fetch policy should be role-invariant; hook=%v empty=%v", pHook.AllowedOps, pEmpty.AllowedOps)
	}
}

func opsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, op := range a {
		if !slices.Contains(b, op) {
			return false
		}
	}
	return true
}
