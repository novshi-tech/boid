package sandbox_test

// docs/plans/signal-ingest-detailed-design.md §3.2 (PR-3): broker.go's
// signal_list / signal_ack / signal_ingest / signal_cursor_get cases.
//
// Workspace scoping for all four ops is broker-injected unconditionally
// (design doc: "引数で workspace を指定させない") — unlike BoidOpCardList's
// three-branch project_id/workspace_id/neither shape, there is no flag to
// widen it at all, so these tests pin the simpler "always overwritten with
// the token's own workspace" invariant instead of a cross-workspace-denied
// test.

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func signalListBoidPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {
			AllowedOps: map[string]struct{}{
				string(sandbox.BoidOpSignalList): {},
				string(sandbox.BoidOpSignalAck):  {},
			},
			AllowedCwdRoots: []string{"/tmp"},
		},
	}
}

// signalIngestBoidPolicies is the PR-5-style reduced policy a connector job
// would carry — used only to exercise the broker's scoping/validation logic
// for signal_ingest/signal_cursor_get ahead of PR-5 actually issuing such a
// policy to real jobs.
func signalIngestBoidPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {
			AllowedOps: map[string]struct{}{
				string(sandbox.BoidOpSignalIngest):    {},
				string(sandbox.BoidOpSignalCursorGet): {},
			},
			AllowedCwdRoots: []string{"/tmp"},
		},
	}
}

func TestBroker_BoidSignalList_PolicyReject(t *testing.T) {
	assertBoidOpRejectedByPolicy(t, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalList})
}

func TestBroker_BoidSignalAck_PolicyReject(t *testing.T) {
	assertBoidOpRejectedByPolicy(t, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalAck, SignalIDs: []string{"sig-1"}})
}

// TestBroker_BoidSignalIngest_PolicyReject / TestBroker_BoidSignalCursorGet_PolicyReject
// pin the design doc's central PR-3 decision (§3.2): a policy that does NOT
// name these ops — i.e. every job in PR-3, since boidPolicy never grants
// them — must reject them just like any other unlisted op. This is the
// "signal_ingest/signal_cursor_get is unreachable from a normal job" gate at
// the broker layer; internal/orchestrator/policy_test.go's
// TestDefaultBuiltinPolicies_HookBoidExcludesConnectorOnlySignalOps pins the
// same boundary at the policy-table layer.
func TestBroker_BoidSignalIngest_PolicyReject(t *testing.T) {
	assertBoidOpRejectedByPolicy(t, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalIngest, Service: "svc", Connector: "pack/conn", IngestPayload: []byte(`{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"x"}`)})
}

func TestBroker_BoidSignalCursorGet_PolicyReject(t *testing.T) {
	assertBoidOpRejectedByPolicy(t, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalCursorGet, Service: "svc", Connector: "pack/conn"})
}

// TestBroker_BoidSignalList_InjectsOwnWorkspace pins the "no argument can
// control workspace" invariant: even a hand-crafted request that sets
// WorkspaceID to something else has it unconditionally overwritten with the
// token's own.
func TestBroker_BoidSignalList_InjectsOwnWorkspace(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{
		JobID:       "j1",
		TaskID:      "t1",
		ProjectID:   "proj-1",
		WorkspaceID: "ws-1",
		Role:        testRoleHook,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalListBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:          sandbox.BoidOpSignalList,
			WorkspaceID: "ws-other",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].WorkspaceID != "ws-1" {
		t.Fatalf("broker should inject the caller's own workspace regardless of the request's own value, calls=%+v", exec.calls)
	}
}

func TestBroker_BoidSignalAck_InjectsOwnWorkspace(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{
		JobID:       "j1",
		TaskID:      "t1",
		ProjectID:   "proj-1",
		WorkspaceID: "ws-1",
		Role:        testRoleHook,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalListBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:          sandbox.BoidOpSignalAck,
			WorkspaceID: "ws-other",
			SignalIDs:   []string{"sig-1"},
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].WorkspaceID != "ws-1" {
		t.Fatalf("broker should inject the caller's own workspace regardless of the request's own value, calls=%+v", exec.calls)
	}
}

// TestBroker_BoidSignalAck_Idempotent drives the SAME ack request through the
// broker twice (Q14) — the fakeBoidExecutor unconditionally returns success,
// so this pins that the broker itself adds no "already acked" state or
// rejection of its own; true idempotency is the store layer's job
// (orchestrator.AckSignals, pinned by TestAckSignals_Idempotent in
// internal/orchestrator/signal_store_test.go) and the executor's job
// (TestBoidBuiltinExecutor_SignalAck_Idempotent in
// internal/server/boid_executor_signal_test.go) — this test closes the
// broker layer of the same "shim/broker/executor 経由" chain.
func TestBroker_BoidSignalAck_Idempotent(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{
		JobID:       "j1",
		TaskID:      "t1",
		ProjectID:   "proj-1",
		WorkspaceID: "ws-1",
		Role:        testRoleHook,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalListBoidPolicies(), ctx)

	req := &sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpSignalAck, SignalIDs: []string{"sig-1"}},
	}
	first := broker.Handle(req)
	if first.ExitCode != 0 {
		t.Fatalf("first ack: exit code = %d, want 0 (stderr=%q)", first.ExitCode, first.Stderr)
	}
	second := broker.Handle(req)
	if second.ExitCode != 0 {
		t.Fatalf("second ack of the same id: exit code = %d, want 0 (stderr=%q)", second.ExitCode, second.Stderr)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("executor calls = %d, want 2", len(exec.calls))
	}
	for i, call := range exec.calls {
		if len(call.SignalIDs) != 1 || call.SignalIDs[0] != "sig-1" {
			t.Errorf("call %d: SignalIDs = %v, want [sig-1]", i, call.SignalIDs)
		}
	}
}

func TestBroker_BoidSignalAck_RequiresAtLeastOneID(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{JobID: "j1", TaskID: "t1", ProjectID: "proj-1", WorkspaceID: "ws-1", Role: testRoleHook}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalListBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpSignalAck},
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection for boid signal ack with no ids")
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be reached, calls=%+v", exec.calls)
	}
}

// --- signal_ingest / signal_cursor_get: scoping + validation, exercised
// against a PR-5-style reduced policy (signalIngestBoidPolicies above) since
// no real job carries one yet in PR-3.
//
// [M2, review of PR #1014, 2026-08-26] Service/Connector are now
// broker-injected from TokenContext.Service/Connector, unconditionally
// overwriting whatever the request carried — the SAME pattern WorkspaceID
// already uses. The tests below set ctx.Service/ctx.Connector (not
// request.Service/Connector) for the success paths, and specifically pin
// that a request trying to claim a DIFFERENT service/connector than its
// token's own gets silently corrected, not honored. ---

// TestBroker_BoidSignalIngest_RequiresServiceAndConnector pins that a
// request is rejected when the TOKEN (not the request) carries no
// service/connector — this is the only way it can be missing now, since the
// broker overwrites the request's own value unconditionally.
func TestBroker_BoidSignalIngest_RequiresServiceAndConnector(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{JobID: "j1", ProjectID: "proj-1", WorkspaceID: "ws-1", Role: testRoleHook}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalIngestBoidPolicies(), ctx)

	// Even a request that itself carries a full service/connector is
	// rejected, because the token backing it has none.
	req := &sandbox.BoidRequest{Op: sandbox.BoidOpSignalIngest, Service: "svc", Connector: "pack/conn", IngestPayload: []byte(`{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"x"}`)}
	resp := broker.Handle(&sandbox.ExecRequest{Command: "boid", Cwd: "/tmp", Token: token, Boid: req})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection: token carries no service/connector")
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be reached, calls=%+v", exec.calls)
	}
}

// TestBroker_BoidSignalIngest_OverwritesRequestServiceConnectorFromToken is
// the core M2 security property: a request that tries to claim a DIFFERENT
// service/connector than its own token's is silently corrected to the
// token's value, not honored or rejected — the same "no argument can widen
// scope" shape WorkspaceID already gets. Before this fix there was no
// TokenContext.Service/Connector at all, so this scenario had no defense.
func TestBroker_BoidSignalIngest_OverwritesRequestServiceConnectorFromToken(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{JobID: "j1", ProjectID: "proj-1", WorkspaceID: "ws-1", Service: "own-svc", Connector: "own-pack/own-conn", Role: testRoleHook}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalIngestBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:            sandbox.BoidOpSignalIngest,
			Service:       "attacker-svc",
			Connector:     "attacker-pack/attacker-conn",
			WorkspaceID:   "ws-other",
			IngestPayload: []byte(`{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"x"}`),
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	got := exec.calls[0]
	if got.Service != "own-svc" || got.Connector != "own-pack/own-conn" || got.WorkspaceID != "ws-1" {
		t.Fatalf("broker should overwrite with the token's own service/connector/workspace, got %+v", got)
	}
}

// TestBroker_BoidSignalIngest_RejectsOversizedPayload is the broker-side half
// of the shim's own 10 MiB stdin cap (design doc §3.2, same
// PayloadPatchMaxBytes reuse as BoidOpTaskUpdatePayloadPatch) — defense in
// depth against a shim bypass.
func TestBroker_BoidSignalIngest_RejectsOversizedPayload(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{JobID: "j1", ProjectID: "proj-1", WorkspaceID: "ws-1", Service: "svc", Connector: "pack/conn", Role: testRoleHook}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalIngestBoidPolicies(), ctx)

	oversized := make([]byte, sandbox.PayloadPatchMaxBytes+1)
	for i := range oversized {
		oversized[i] = 'x'
	}

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:            sandbox.BoidOpSignalIngest,
			IngestPayload: oversized,
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "exceeds") {
		t.Fatalf("expected oversized-payload rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not receive the oversized request, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidSignalCursorGet_RequiresServiceAndConnector(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{JobID: "j1", ProjectID: "proj-1", WorkspaceID: "ws-1", Role: testRoleHook}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalIngestBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpSignalCursorGet, Service: "svc", Connector: "pack/conn"},
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection: token carries no service/connector")
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be reached, calls=%+v", exec.calls)
	}
}

func TestBroker_BoidSignalCursorGet_OverwritesRequestServiceConnectorFromToken(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{JobID: "j1", ProjectID: "proj-1", WorkspaceID: "ws-1", Service: "own-svc", Connector: "own-pack/own-conn", Role: testRoleHook}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalIngestBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:          sandbox.BoidOpSignalCursorGet,
			Service:     "attacker-svc",
			Connector:   "attacker-pack/attacker-conn",
			WorkspaceID: "ws-other",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	got := exec.calls[0]
	if got.Service != "own-svc" || got.Connector != "own-pack/own-conn" || got.WorkspaceID != "ws-1" {
		t.Fatalf("broker should overwrite with the token's own service/connector/workspace, got %+v", got)
	}
}

// TestBroker_BoidSignalIngest_RoundTripsPayload is a light sanity check that
// the broker forwards IngestPayload byte-for-byte to the executor rather
// than transforming it — json round-trip through ExecRequest's own encoding
// (used by the real socket transport) is exercised separately by
// boid_shim_signal_test.go; this only pins the broker's in-process Handle()
// path.
func TestBroker_BoidSignalIngest_RoundTripsPayload(t *testing.T) {
	broker, exec := newBrokerForListTest(t)
	ctx := sandbox.TokenContext{JobID: "j1", ProjectID: "proj-1", WorkspaceID: "ws-1", Service: "svc", Connector: "pack/conn", Role: testRoleHook}
	token := broker.Register(map[string]sandbox.CommandDef{}, signalIngestBoidPolicies(), ctx)

	payload := []byte(`{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"x"}` + "\n")
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:            sandbox.BoidOpSignalIngest,
			IngestPayload: payload,
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || string(exec.calls[0].IngestPayload) != string(payload) {
		t.Fatalf("IngestPayload not forwarded verbatim: %+v", exec.calls)
	}
}
