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

// hook×git policy は gate×git と等価 (fetch/push を含む)。
func TestDefaultBuiltinPolicies_HookGitHasFetchPush(t *testing.T) {
	gitPolicy := DefaultBuiltinPolicies(RoleHook, []string{"git"}, PolicyContext{})["git"]
	if !gitPolicy.Allows(OpGitFetch) {
		t.Error("hook×git should allow fetch")
	}
	if !gitPolicy.Allows(OpGitPush) {
		t.Error("hook×git should allow push")
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

// gate×git policy は cwd に /tmp, ProjectDir, HomeDir を許可する。
// gate sandbox は worktree を mount しないため cwd は必ずホスト worktree と
// 別名前空間になる。broker 側の cwd check を通すため policy で明示許可する。
func TestDefaultBuiltinPolicies_GateGitCwdRoots(t *testing.T) {
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: "/home/user"}
	gitPolicy := DefaultBuiltinPolicies(RoleGate, []string{"git"}, pctx)["git"]
	for _, cwd := range []string{"/tmp", "/work/project", "/home/user", "/home/user/nested"} {
		if !gitPolicy.AllowsCwd(cwd) {
			t.Errorf("gate×git should allow cwd %q, AllowedCwdRoots=%v", cwd, gitPolicy.AllowedCwdRoots)
		}
	}
	if gitPolicy.AllowsCwd("/etc") {
		t.Errorf("gate×git should reject cwd /etc")
	}
}

// hook×boid policy は gate×boid と等価 (全 op を含む)。
func TestDefaultBuiltinPolicies_HookBoidOps(t *testing.T) {
	hookBoid := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})["boid"]
	gateBoid := DefaultBuiltinPolicies(RoleGate, []string{"boid"}, PolicyContext{})["boid"]
	if !opsEqual(hookBoid.AllowedOps, gateBoid.AllowedOps) {
		t.Errorf("hook×boid AllowedOps = %v, want gate-equivalent %v", hookBoid.AllowedOps, gateBoid.AllowedOps)
	}
}

// gate×boid policy は全 op を含む。
func TestDefaultBuiltinPolicies_GateBoidOps(t *testing.T) {
	boidPolicy := DefaultBuiltinPolicies(RoleGate, []string{"boid"}, PolicyContext{})["boid"]
	wantOps := []string{
		OpBoidJobDone,
		OpBoidJobList,
		OpBoidJobShow,
		OpBoidJobLog,
		OpBoidActionSend,
		OpBoidTaskCreate,
		OpBoidTaskGet,
		OpBoidTaskUpdate,
		OpBoidTaskImport,
		OpBoidTaskReopen,
		OpBoidTaskList,
		OpBoidTaskNotify,
		OpBoidTaskAnswer,
		OpBoidTaskDelete,
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

// RoleHook と RoleGate の builtin policy は完全に等価でなければならない。
// 将来分岐させたくなった時の回帰防止テスト。
func TestDefaultBuiltinPolicies_HookEqualsGate(t *testing.T) {
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: "/home/user"}
	for _, name := range []string{"boid", "git"} {
		hookP := DefaultBuiltinPolicies(RoleHook, []string{name}, pctx)[name]
		gateP := DefaultBuiltinPolicies(RoleGate, []string{name}, pctx)[name]
		if !opsEqual(hookP.AllowedOps, gateP.AllowedOps) {
			t.Errorf("%s: hook AllowedOps = %v, gate = %v (should be equal)", name, hookP.AllowedOps, gateP.AllowedOps)
		}
		if !slices.Equal(hookP.AllowedCwdRoots, gateP.AllowedCwdRoots) {
			t.Errorf("%s: hook AllowedCwdRoots = %v, gate = %v (should be equal)", name, hookP.AllowedCwdRoots, gateP.AllowedCwdRoots)
		}
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
