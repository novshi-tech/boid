package dispatcher

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// orchestrator のミラー定数が sandbox の正典定数と一致することを保証する。
// dispatcher は両方を import できる唯一のレイヤなので、ここでドリフトを捕捉する。
func TestOpConstantsMirror(t *testing.T) {
	pairs := []struct {
		orchestratorConst string
		sandboxConst      string
	}{
		{orchestrator.OpBoidJobDone, string(sandbox.BoidOpJobDone)},
		{orchestrator.OpBoidTaskCreate, string(sandbox.BoidOpTaskCreate)},
		{orchestrator.OpBoidTaskGet, string(sandbox.BoidOpTaskGet)},
		{orchestrator.OpBoidTaskUpdate, string(sandbox.BoidOpTaskUpdate)},
		{orchestrator.OpBoidTaskImport, string(sandbox.BoidOpTaskImport)},
		{orchestrator.OpBoidTaskReopen, string(sandbox.BoidOpTaskReopen)},
		{orchestrator.OpGitFetch, string(sandbox.GitOpFetch)},
		{orchestrator.OpGitPush, string(sandbox.GitOpPush)},
	}
	for _, p := range pairs {
		if p.orchestratorConst != p.sandboxConst {
			t.Errorf("op constant drift: orchestrator=%q sandbox=%q", p.orchestratorConst, p.sandboxConst)
		}
	}
}

func TestPoliciesToSandbox_RoundTrip(t *testing.T) {
	in := map[string]orchestrator.BuiltinPolicy{
		"boid": {
			AllowedOps:      []string{orchestrator.OpBoidJobDone, orchestrator.OpBoidTaskGet},
			AllowedCwdRoots: []string{"/tmp", "/workspace"},
		},
		"git": {
			AllowedOps: []string{orchestrator.OpGitFetch, orchestrator.OpGitPush},
		},
		"empty": {},
	}
	out := PoliciesToSandbox(in)
	if len(out) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(out))
	}
	boid := out["boid"]
	if _, ok := boid.AllowedOps[string(sandbox.BoidOpJobDone)]; !ok {
		t.Error("boid policy missing BoidOpJobDone")
	}
	if _, ok := boid.AllowedOps[string(sandbox.BoidOpTaskGet)]; !ok {
		t.Error("boid policy missing BoidOpTaskGet")
	}
	if len(boid.AllowedCwdRoots) != 2 {
		t.Errorf("boid policy AllowedCwdRoots = %v, want 2 entries", boid.AllowedCwdRoots)
	}
	if !out["git"].Allows(string(sandbox.GitOpFetch)) {
		t.Error("git policy should allow fetch")
	}
	if !out["git"].Allows(string(sandbox.GitOpPush)) {
		t.Error("git policy should allow push")
	}
	if len(out["empty"].AllowedOps) != 0 {
		t.Errorf("empty policy should have 0 ops, got %d", len(out["empty"].AllowedOps))
	}
}

func TestPoliciesToSandbox_NilInput(t *testing.T) {
	if got := PoliciesToSandbox(nil); got != nil {
		t.Errorf("PoliciesToSandbox(nil) = %v, want nil", got)
	}
	if got := PoliciesToSandbox(map[string]orchestrator.BuiltinPolicy{}); got != nil {
		t.Errorf("PoliciesToSandbox(empty) = %v, want nil", got)
	}
}
