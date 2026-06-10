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

func TestDefaultBuiltinPolicies_HookBoidOnly(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"boid"}, PolicyContext{})
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if _, ok := policies["boid"]; !ok {
		t.Error("missing boid policy")
	}
}

func TestDefaultBuiltinPolicies_HookGitHasFetchPush(t *testing.T) {
	gitPolicy := DefaultBuiltinPolicies(RoleHook, []string{"git"}, PolicyContext{})["git"]
	if !gitPolicy.Allows(OpGitFetch) {
		t.Error("hook×git should allow fetch")
	}
	if !gitPolicy.Allows(OpGitPush) {
		t.Error("hook×git should allow push")
	}
}

// hook×git policy は cwd に /tmp と ProjectDir のみを許可する。
// HomeDir は意図的に除外（peer project への git plumbing 書き込みを防ぐため）。
func TestDefaultBuiltinPolicies_HookGitCwdRoots(t *testing.T) {
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: "/home/user"}
	gitPolicy := DefaultBuiltinPolicies(RoleHook, []string{"git"}, pctx)["git"]
	for _, cwd := range []string{"/tmp", "/tmp/subdir", "/work/project", "/work/project/sub"} {
		if !gitPolicy.AllowsCwd(cwd) {
			t.Errorf("hook×git should allow cwd %q, AllowedCwdRoots=%v", cwd, gitPolicy.AllowedCwdRoots)
		}
	}
	for _, cwd := range []string{"/home/user", "/home/user/nested", "/etc"} {
		if gitPolicy.AllowsCwd(cwd) {
			t.Errorf("hook×git should reject cwd %q", cwd)
		}
	}
}

// git policy は HomeDir を AllowedCwdRoots に含まない。
func TestDefaultBuiltinPolicies_GitPolicyExcludesHomeDir(t *testing.T) {
	homeDir := "/home/testuser"
	pctx := PolicyContext{ProjectDir: "/work/project", HomeDir: homeDir}
	for _, role := range []Role{RoleHook, ""} {
		p := DefaultBuiltinPolicies(role, []string{"git"}, pctx)["git"]
		for _, root := range p.AllowedCwdRoots {
			if root == homeDir {
				t.Errorf("role=%q: git policy AllowedCwdRoots contains HomeDir %q (must be excluded)", role, homeDir)
			}
		}
		peer := homeDir + "/src/peer-project"
		if p.AllowsCwd(peer) {
			t.Errorf("role=%q: git policy allows peer project cwd %q (HomeDir must not be in AllowedCwdRoots)", role, peer)
		}
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
