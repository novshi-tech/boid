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

// gate×git policy は cwd に /tmp と ProjectDir のみを許可する。
// HomeDir は意図的に除外（peer project への git plumbing 書き込みを防ぐため）。
// WorktreeRoot 配下は validateGitBuiltinCwd が独立チェックするため policy 不要。
func TestDefaultBuiltinPolicies_GateGitCwdRoots(t *testing.T) {
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: "/home/user"}
	gitPolicy := DefaultBuiltinPolicies(RoleGate, []string{"git"}, pctx)["git"]
	for _, cwd := range []string{"/tmp", "/tmp/subdir", "/work/project", "/work/project/sub"} {
		if !gitPolicy.AllowsCwd(cwd) {
			t.Errorf("gate×git should allow cwd %q, AllowedCwdRoots=%v", cwd, gitPolicy.AllowedCwdRoots)
		}
	}
	// HomeDir および HomeDir 配下 (peer project 等) は拒否される。
	for _, cwd := range []string{"/home/user", "/home/user/nested", "/etc"} {
		if gitPolicy.AllowsCwd(cwd) {
			t.Errorf("gate×git should reject cwd %q", cwd)
		}
	}
}

// git policy は HomeDir を AllowedCwdRoots に含まない。
// 含まれていると peer project 配下の cwd が通り、plumbing 経由で
// peer project に書き込めてしまう (security hole)。
func TestDefaultBuiltinPolicies_GitPolicyExcludesHomeDir(t *testing.T) {
	homeDir := "/home/testuser"
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: homeDir}
	for _, role := range []Role{RoleGate, RoleHook, ""} {
		p := DefaultBuiltinPolicies(role, []string{"git"}, pctx)["git"]
		for _, root := range p.AllowedCwdRoots {
			if root == homeDir {
				t.Errorf("role=%q: git policy AllowedCwdRoots contains HomeDir %q (must be excluded)", role, homeDir)
			}
		}
		// peer project under HomeDir must be rejected by AllowsCwd
		peer := homeDir + "/src/peer-project"
		if p.AllowsCwd(peer) {
			t.Errorf("role=%q: git policy allows peer project cwd %q (HomeDir must not be in AllowedCwdRoots)", role, peer)
		}
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
		OpBoidAgentStop,
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
