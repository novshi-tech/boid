package sandbox_test

// docs/plans/signal-ingest-detailed-design.md §5.2 (PR-5), Q27: "connector
// job の builtin op は signal 系 (ingest / cursor) のみに制限され、他の op
// と宣言外 service への gateway 到達が拒否されるテストがある". Unlike
// broker_signal_test.go's signalIngestBoidPolicies() (a hand-rolled test
// double built "ahead of PR-5 actually issuing such a policy to real jobs"
// — see that helper's own doc comment), this file drives the REAL policy a
// connector job gets: orchestrator.ConnectorBuiltinPolicies, translated
// through dispatcher.PoliciesToSandbox exactly the way
// dispatcher.BuildSessionJobSpec does — proving the policy-table layer
// (internal/orchestrator/policy_test.go) and the broker-enforcement layer
// agree on the SAME real value, not two independently-maintained doubles.

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// realConnectorPolicies builds the exact map dispatcher.BuildSessionJobSpec
// installs on a connector job's JobSpec.BuiltinPolicies when
// SessionJobInput.ConnectorPolicy is true.
func realConnectorPolicies() map[string]sandbox.BuiltinPolicy {
	return dispatcher.PoliciesToSandbox(orchestrator.ConnectorBuiltinPolicies(orchestrator.PolicyContext{ProjectDir: "/tmp"}))
}

// TestBroker_ConnectorPolicy_AllowsSignalIngestAndCursorGet pins the
// positive half of Q27: a connector job's real, production-shaped policy
// lets signal_ingest/signal_cursor_get reach the executor.
func TestBroker_ConnectorPolicy_AllowsSignalIngestAndCursorGet(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{
		JobID: "j1", TaskID: "t1", ProjectID: "proj-1", WorkspaceID: "ws-1",
		Role: testRoleHook, Service: "slack-api", Connector: "slack/mentions",
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, realConnectorPolicies(), ctx)

	ingestResp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid", Cwd: "/tmp", Token: token,
		Boid: &sandbox.BoidRequest{
			Op:            sandbox.BoidOpSignalIngest,
			IngestPayload: []byte(`{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"x"}`),
		},
	})
	if ingestResp.ExitCode != 0 {
		t.Fatalf("signal_ingest: exit=%d stderr=%q, want allowed by the real connector policy", ingestResp.ExitCode, ingestResp.Stderr)
	}

	cursorResp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid", Cwd: "/tmp", Token: token,
		Boid: &sandbox.BoidRequest{Op: sandbox.BoidOpSignalCursorGet},
	})
	if cursorResp.ExitCode != 0 {
		t.Fatalf("signal_cursor_get: exit=%d stderr=%q, want allowed by the real connector policy", cursorResp.ExitCode, cursorResp.Stderr)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("executor calls = %d, want 2 (both ops reached it)", len(exec.calls))
	}
}

// TestBroker_ConnectorPolicy_RejectsGeneralBoidOps pins the negative half of
// Q27: task_create (and every other general boidPolicy op) is rejected by
// policy BEFORE reaching the executor — a connector job cannot touch boid
// state beyond its own source's ingest/cursor.
func TestBroker_ConnectorPolicy_RejectsGeneralBoidOps(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{
		JobID: "j1", TaskID: "t1", ProjectID: "proj-1", WorkspaceID: "ws-1",
		Role: testRoleHook, Service: "slack-api", Connector: "slack/mentions",
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, realConnectorPolicies(), ctx)

	forbidden := []*sandbox.BoidRequest{
		{Op: sandbox.BoidOpTaskCreate},
		{Op: sandbox.BoidOpSignalList},
		{Op: sandbox.BoidOpSignalAck, SignalIDs: []string{"sig-1"}},
		{Op: sandbox.BoidOpCardList},
		{Op: sandbox.BoidOpActionSend},
	}
	for _, req := range forbidden {
		resp := broker.Handle(&sandbox.ExecRequest{Command: "boid", Cwd: "/tmp", Token: token, Boid: req})
		if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed by policy") {
			t.Errorf("op %q: expected policy rejection, got exit=%d stderr=%q", req.Op, resp.ExitCode, resp.Stderr)
		}
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should never be reached for a rejected op, got %d calls: %+v", len(exec.calls), exec.calls)
	}
}

// TestBroker_ConnectorPolicy_NoFetchBuiltin pins that a connector job's
// policy map carries no "fetch" entry at all — the broker rejects a "fetch"
// command outright for a token that never registered it (mirrors how "boid"
// itself would be rejected if omitted; here it is "fetch" that is omitted).
func TestBroker_ConnectorPolicy_NoFetchBuiltin(t *testing.T) {
	policies := realConnectorPolicies()
	if _, ok := policies["fetch"]; ok {
		t.Fatal(`connector policy must not carry a "fetch" entry (§5.2: connector の外部到達は gateway で足りる)`)
	}
}
