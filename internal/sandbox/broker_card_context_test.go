package sandbox_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// The broker never lets a caller supply card/request identity for
// BoidOpCardContext — it comes exclusively from the token entry (same
// "broker fills context from the token, never the caller's self-report"
// pattern the Connector fields use for signal ops). A job with no card
// context is rejected before it ever reaches the executor.

func testCardContextBoidPolicy() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpCardContext): {},
		}},
	}
}

func TestBroker_BoidCardContext_NoCardContext_RejectedBeforeExecutor(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testCardContextBoidPolicy(), sandbox.TokenContext{
		JobID:      "job-1",
		ProjectID:  "proj-1",
		ProjectDir: projectDir,
		// CardID/CardRequestID deliberately left empty — every job today.
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext},
	})

	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "no card context") {
		t.Fatalf("expected a clear no-card-context rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should never be reached when the token has no card context, calls=%d", len(exec.calls))
	}
}

// TestBroker_BoidCardContext_TokenContextReachesExecutor registers a token
// WITH card context and confirms the executor is invoked with exactly that
// token's CardID/CardRequestID — never anything a caller could plant on the
// request, since BoidRequest carries no card fields at all for this op.
func TestBroker_BoidCardContext_TokenContextReachesExecutor(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testCardContextBoidPolicy(), sandbox.TokenContext{
		JobID:         "job-1",
		ProjectID:     "proj-1",
		ProjectDir:    projectDir,
		CardID:        "card-1",
		CardRequestID: "req-1",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext},
	})

	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.ctxCalls) != 1 {
		t.Fatalf("expected exactly one executor call, got %d", len(exec.ctxCalls))
	}
	got := exec.ctxCalls[0]
	if got.CardID != "card-1" || got.CardRequestID != "req-1" {
		t.Errorf("executor ctx = %+v, want CardID=card-1 CardRequestID=req-1", got)
	}
}

func TestBroker_BoidCardContext_DisallowedByPolicy_Rejected(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	// No "boid" builtin policy registered at all.
	token := broker.Register(map[string]sandbox.CommandDef{}, nil, sandbox.TokenContext{
		JobID:         "job-1",
		ProjectID:     "proj-1",
		ProjectDir:    projectDir,
		CardID:        "card-1",
		CardRequestID: "req-1",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext},
	})

	if resp.ExitCode != 1 {
		t.Fatalf("expected rejection without the boid builtin policy, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be reached, calls=%d", len(exec.calls))
	}
}
