package dispatcher

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// orchestrator のミラー定数が sandbox の正典定数と一致することを保証する。
// dispatcher は両レイヤを import できる唯一のレイヤなので、ここでドリフトを捕捉する。
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
		{orchestrator.OpBoidTaskList, string(sandbox.BoidOpTaskList)},
		{orchestrator.OpBoidTaskNotify, string(sandbox.BoidOpTaskNotify)},
		{orchestrator.OpBoidTaskAnswer, string(sandbox.BoidOpTaskAnswer)},
		{orchestrator.OpBoidTaskAsk, string(sandbox.BoidOpTaskAsk)},
		{orchestrator.OpBoidTaskDelete, string(sandbox.BoidOpTaskDelete)},
		{orchestrator.OpBoidTaskCurrent, string(sandbox.BoidOpTaskCurrent)},
		{orchestrator.OpBoidTaskInstructions, string(sandbox.BoidOpTaskInstructions)},
		{orchestrator.OpBoidTaskEnv, string(sandbox.BoidOpTaskEnv)},
		{orchestrator.OpBoidTaskPayload, string(sandbox.BoidOpTaskPayload)},
		{orchestrator.OpBoidTaskAttachmentsList, string(sandbox.BoidOpTaskAttachmentsList)},
		{orchestrator.OpBoidTaskAttachmentsGet, string(sandbox.BoidOpTaskAttachmentsGet)},
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
		"fetch": {
			AllowedOps: []string{orchestrator.OpFetchGet},
		},
		"empty": {},
	}
	out := PoliciesToSandbox(in)
	if len(out) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(out))
	}
	if _, ok := out["boid"].AllowedOps[string(sandbox.BoidOpJobDone)]; !ok {
		t.Error("boid policy missing BoidOpJobDone")
	}
	if len(out["boid"].AllowedCwdRoots) != 2 {
		t.Errorf("boid policy AllowedCwdRoots = %v, want 2 entries", out["boid"].AllowedCwdRoots)
	}
	if !out["fetch"].Allows(orchestrator.OpFetchGet) {
		t.Error("fetch policy should allow get")
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
