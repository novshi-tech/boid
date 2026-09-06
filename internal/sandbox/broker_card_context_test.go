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

// TestBroker_BoidCardContext_SelfReportedIDsAreIgnored pins the "request ID
// の自己申告だけで操作権限を与えない" contract: a hand-crafted request that
// tries to smuggle CardID/CardRequestID through BoidRequest itself (fields
// BoidRequest doesn't even expose for this op, but the executor would still
// only ever see whatever the broker forwards) never overrides the token's
// own context. Registers a token WITH card context and confirms the
// executor receives that token's ids rather than anything a caller could
// plant on the request.
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
	if len(exec.calls) != 1 {
		t.Fatalf("expected exactly one executor call, got %d", len(exec.calls))
	}
	// BoidRequest carries no card fields at all for this op — identity is
	// TokenContext-only, verified separately at the executor layer
	// (boid_executor_card_context_test.go), which reads ctx.CardID/
	// ctx.CardRequestID directly rather than anything on the request.
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
