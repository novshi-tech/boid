package orchestrator

import (
	"slices"
	"testing"
)

func TestDefaultBuiltinPolicies_HookGitBoid(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"git", "boid"}, PolicyContext{})
	if len(policies) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(policies))
	}
	if _, ok := policies["git"]; !ok {
		t.Error("missing git policy")
	}
	if _, ok := policies["boid"]; !ok {
		t.Error("missing boid policy")
	}
}

func TestDefaultBuiltinPolicies_GateGitBoid(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleGate, []string{"git", "boid"}, PolicyContext{})
	if len(policies) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(policies))
	}
	if _, ok := policies["git"]; !ok {
		t.Error("missing git policy")
	}
	if _, ok := policies["boid"]; !ok {
		t.Error("missing boid policy")
	}
}

func TestDefaultBuiltinPolicies_EmptyRoleEqualsGate(t *testing.T) {
	gateGit := DefaultBuiltinPolicies(RoleGate, []string{"git"}, PolicyContext{})
	defaultGit := DefaultBuiltinPolicies("", []string{"git"}, PolicyContext{})
	gateBoid := DefaultBuiltinPolicies(RoleGate, []string{"boid"}, PolicyContext{})
	defaultBoid := DefaultBuiltinPolicies("", []string{"boid"}, PolicyContext{})

	if !opsEqual(gateGit["git"].AllowedOps, defaultGit["git"].AllowedOps) {
		t.Error("default git policy should equal gate git policy")
	}
	if !opsEqual(gateBoid["boid"].AllowedOps, defaultBoid["boid"].AllowedOps) {
		t.Error("default boid policy should equal gate boid policy")
	}
}

func TestDefaultBuiltinPolicies_HookBoidOnly(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if _, ok := policies["boid"]; !ok {
		t.Error("missing boid policy")
	}
}

func TestDefaultBuiltinPolicies_GateGitOnly(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleGate, []string{"git"}, PolicyContext{})
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if _, ok := policies["git"]; !ok {
		t.Error("missing git policy")
	}
}

// hook×git policy は AllowedOps が空 (broker 経由 git は hook から禁止)。
func TestDefaultBuiltinPolicies_HookGitIsEmpty(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"git"}, PolicyContext{})
	if len(policies["git"].AllowedOps) != 0 {
		t.Errorf("hook×git AllowedOps should be empty, got %v", policies["git"].AllowedOps)
	}
}

// gate×git policy は fetch/push を含む。
func TestDefaultBuiltinPolicies_GateGitHasFetchPush(t *testing.T) {
	gitPolicy := DefaultBuiltinPolicies(RoleGate, []string{"git"}, PolicyContext{})["git"]
	if !gitPolicy.Allows(OpGitFetch) {
		t.Error("gate×git should allow fetch")
	}
	if !gitPolicy.Allows(OpGitPush) {
		t.Error("gate×git should allow push")
	}
}

// hook×boid policy は {job_done, task_get}。
func TestDefaultBuiltinPolicies_HookBoidOps(t *testing.T) {
	boidPolicy := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})["boid"]
	wantOps := []string{OpBoidJobDone, OpBoidTaskGet}
	if !opsEqual(boidPolicy.AllowedOps, wantOps) {
		t.Errorf("hook×boid AllowedOps = %v, want %v", boidPolicy.AllowedOps, wantOps)
	}
}

// gate×boid policy は {job_done, task_create, task_update, task_import, task.reopen}。
func TestDefaultBuiltinPolicies_GateBoidOps(t *testing.T) {
	boidPolicy := DefaultBuiltinPolicies(RoleGate, []string{"boid"}, PolicyContext{})["boid"]
	wantOps := []string{
		OpBoidJobDone,
		OpBoidTaskCreate,
		OpBoidTaskUpdate,
		OpBoidTaskImport,
		OpBoidTaskReopen,
	}
	if !opsEqual(boidPolicy.AllowedOps, wantOps) {
		t.Errorf("gate×boid AllowedOps = %v, want %v", boidPolicy.AllowedOps, wantOps)
	}
}

// gate×boid policy は cwd に /tmp, ProjectDir, HomeDir を許可する。
// gate sandbox のデフォルト cwd は HOME (resolveWorkDir フォールバック) なので、
// HomeDir を policy で認めないと exit trap の `boid job done` が弾かれる。
func TestDefaultBuiltinPolicies_GateBoidCwdRoots(t *testing.T) {
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: "/home/user"}
	boidPolicy := DefaultBuiltinPolicies(RoleGate, []string{"boid"}, pctx)["boid"]
	for _, cwd := range []string{"/tmp", "/tmp/sub", "/work/project", "/work/project/sub", "/home/user", "/home/user/.boid/output"} {
		if !boidPolicy.AllowsCwd(cwd) {
			t.Errorf("gate×boid should allow cwd %q, AllowedCwdRoots=%v", cwd, boidPolicy.AllowedCwdRoots)
		}
	}
	if boidPolicy.AllowsCwd("/etc") {
		t.Errorf("gate×boid should reject cwd /etc")
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
