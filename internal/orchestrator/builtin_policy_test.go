package orchestrator

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestDefaultBuiltinPolicies_HookGitBoid(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"git", "boid"})
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
	policies := DefaultBuiltinPolicies(RoleGate, []string{"git", "boid"})
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
	// テスト互換のため gate と同じ policy を返す。
	// production 経路では Role は必ず設定される。
	gateGit := DefaultBuiltinPolicies(RoleGate, []string{"git"})
	defaultGit := DefaultBuiltinPolicies("", []string{"git"})

	gateBoid := DefaultBuiltinPolicies(RoleGate, []string{"boid"})
	defaultBoid := DefaultBuiltinPolicies("", []string{"boid"})

	if !opsEqual(gateGit["git"].AllowedOps, defaultGit["git"].AllowedOps) {
		t.Error("default git policy should equal gate git policy")
	}
	if !opsEqual(gateBoid["boid"].AllowedOps, defaultBoid["boid"].AllowedOps) {
		t.Error("default boid policy should equal gate boid policy")
	}
}

func TestDefaultBuiltinPolicies_HookBoidOnly(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"boid"})
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if _, ok := policies["boid"]; !ok {
		t.Error("missing boid policy")
	}
}

func TestDefaultBuiltinPolicies_GateGitOnly(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleGate, []string{"git"})
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if _, ok := policies["git"]; !ok {
		t.Error("missing git policy")
	}
}

// hook×git policy は AllowedOps が空であること。
// hook からの broker 経由 git 操作は禁止。agent はホスト側リモートに直接アクセスさせない。
func TestDefaultBuiltinPolicies_HookGitIsEmpty(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"git"})
	gitPolicy := policies["git"]
	if len(gitPolicy.AllowedOps) != 0 {
		t.Errorf("hook×git AllowedOps should be empty, got %v", gitPolicy.AllowedOps)
	}
}

// gate×git policy は {fetch, push} を含むこと。
// gate は fetch/push 両方を使う (pr-verify での push, worktree 作成時の fetch 等)。
func TestDefaultBuiltinPolicies_GateGitHasFetchPush(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleGate, []string{"git"})
	gitPolicy := policies["git"]
	if !gitPolicy.Allows(string(sandbox.GitOpFetch)) {
		t.Error("gate×git should allow fetch")
	}
	if !gitPolicy.Allows(string(sandbox.GitOpPush)) {
		t.Error("gate×git should allow push")
	}
}

// hook×boid policy は {job_done, task_get} であること。
// agent は task を作成/更新しない。読み取り専用操作 (task_get) と完了通知 (job_done) のみ許可。
func TestDefaultBuiltinPolicies_HookBoidOps(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleHook, []string{"boid"})
	boidPolicy := policies["boid"]

	wantOps := map[string]struct{}{
		string(sandbox.BoidOpJobDone):  {},
		string(sandbox.BoidOpTaskGet):  {},
	}
	if !opsEqual(boidPolicy.AllowedOps, wantOps) {
		t.Errorf("hook×boid AllowedOps = %v, want {job_done, task_get}", boidPolicy.AllowedOps)
	}
}

// gate×boid policy は {job_done, task_create, task_update, task_import, task.reopen} であること。
// gate は verification 結果を task に反映する必要があるため task_create/task_update を許可。
// task.reopen は detect-conflicts kit がコンフリクト検出時に done タスクを reworking に戻すために必要。
func TestDefaultBuiltinPolicies_GateBoidOps(t *testing.T) {
	policies := DefaultBuiltinPolicies(RoleGate, []string{"boid"})
	boidPolicy := policies["boid"]

	wantOps := map[string]struct{}{
		string(sandbox.BoidOpJobDone):    {},
		string(sandbox.BoidOpTaskCreate): {},
		string(sandbox.BoidOpTaskUpdate): {},
		string(sandbox.BoidOpTaskImport): {},
		string(sandbox.BoidOpTaskReopen): {},
	}
	if !opsEqual(boidPolicy.AllowedOps, wantOps) {
		t.Errorf("gate×boid AllowedOps = %v, want {job_done, task_create, task_update, task_import, task.reopen}", boidPolicy.AllowedOps)
	}
}

// opsEqual は2つの map[string]struct{} が同じ内容か比較する。
func opsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
