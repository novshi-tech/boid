package server

// docs/plans/signal-ingest-detailed-design.md §3.2 (PR-3): boid_executor.go's
// signal_list / signal_ack / signal_ingest / signal_cursor_get cases.
// Broker-side scoping is covered separately (internal/sandbox's
// TestBroker_BoidSignal*); these pin the executor's own logic: the --claim
// vs plain-list branch, the "unavailable" guard when the workflow value
// doesn't implement api.SignalStore, and (Q14) that repeating an ack is
// harmless end to end through this layer too.

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// fakeSignalStore is a minimal api.SignalStore double. AckSignals mimics the
// real store's idempotent semantics (orchestrator.AckSignals: acked_at set
// WHERE acked_at IS NULL — an already-acked id is a no-op success) closely
// enough to prove the executor doesn't add its own "already acked" error on
// a repeat call.
type fakeSignalStore struct {
	listCalls  []orchestrator.SignalFilter
	listResult []*orchestrator.Signal
	listErr    error
	claimCalls []struct {
		workspaceID        string
		limit, maxAttempts int
	}
	claimResult []*orchestrator.Signal
	claimErr    error
	ackCalls    [][]string
	ackedIDs    map[string]bool
	ackErr      error
	ingestCalls []struct {
		workspaceID, service, connector string
		rows                            []orchestrator.SignalIngestRow
	}
	ingestErr error
	cursorArg struct{ workspaceID, service, connector string }
	cursor    string
	cursorErr error
}

func (f *fakeSignalStore) ListSignals(filter orchestrator.SignalFilter) ([]*orchestrator.Signal, error) {
	f.listCalls = append(f.listCalls, filter)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeSignalStore) ClaimSignals(workspaceID string, limit, maxAttempts int) ([]*orchestrator.Signal, error) {
	f.claimCalls = append(f.claimCalls, struct {
		workspaceID string
		limit       int
		maxAttempts int
	}{workspaceID, limit, maxAttempts})
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	return f.claimResult, nil
}

func (f *fakeSignalStore) AckSignals(workspaceID string, ids []string) error {
	f.ackCalls = append(f.ackCalls, ids)
	if f.ackErr != nil {
		return f.ackErr
	}
	if f.ackedIDs == nil {
		f.ackedIDs = map[string]bool{}
	}
	for _, id := range ids {
		f.ackedIDs[id] = true // idempotent: re-acking is harmless, no error
	}
	return nil
}

func (f *fakeSignalStore) IngestSignals(workspaceID, service, connector string, rows []orchestrator.SignalIngestRow) error {
	f.ingestCalls = append(f.ingestCalls, struct {
		workspaceID, service, connector string
		rows                            []orchestrator.SignalIngestRow
	}{workspaceID, service, connector, rows})
	return f.ingestErr
}

func (f *fakeSignalStore) GetSignalCursor(workspaceID, service, connector string) (string, error) {
	f.cursorArg = struct{ workspaceID, service, connector string }{workspaceID, service, connector}
	return f.cursor, f.cursorErr
}

func (f *fakeSignalStore) HasPendingSignals(workspaceID string, maxAttempts int) (bool, error) {
	return false, nil
}

// --- signal_list ---

func TestBoidBuiltinExecutor_SignalList_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{} // signals left nil
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalList})
	if resp.ExitCode != 1 || resp.Stderr == "" {
		t.Fatalf("expected an unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_SignalList_RequiresWorkspace(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalList})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection for a missing workspace")
	}
	if len(fake.listCalls) != 0 {
		t.Fatalf("store should not be called, calls=%+v", fake.listCalls)
	}
}

func TestBoidBuiltinExecutor_SignalList_PlainListForwardsFilter(t *testing.T) {
	fake := &fakeSignalStore{listResult: []*orchestrator.Signal{{WorkspaceID: "ws-1", ID: "sig-1"}}}
	exec := &boidBuiltinExecutor{signals: fake}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpSignalList,
		WorkspaceID: "ws-1",
		Connector:   "slack/mentions",
		SignalState: "dead",
		Limit:       50,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(fake.listCalls) != 1 {
		t.Fatalf("ListSignals calls = %d, want 1", len(fake.listCalls))
	}
	got := fake.listCalls[0]
	if got.WorkspaceID != "ws-1" || got.Connector != "slack/mentions" || got.State != orchestrator.SignalStateDead || got.Limit != 50 {
		t.Errorf("filter = %+v, unexpected", got)
	}
	if len(fake.claimCalls) != 0 {
		t.Fatalf("ClaimSignals should not be called for a plain list, calls=%+v", fake.claimCalls)
	}
	// orchestrator.Signal (signal_store.go, PR-1) has no json tags, so its
	// default JSON rendering uses the capitalized Go field names verbatim —
	// unlike api.CardView/api.ActionListResult (which have their own json
	// tags in internal/api), there is no api-level wrapper type for Signal
	// in scope for this PR to introduce one.
	if !strings.Contains(resp.Stdout, `"ID": "sig-1"`) {
		t.Errorf("stdout missing signal id: %s", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_SignalList_ClaimCallsClaimSignals(t *testing.T) {
	fake := &fakeSignalStore{claimResult: []*orchestrator.Signal{{WorkspaceID: "ws-1", ID: "sig-1", Attempts: 1}}}
	exec := &boidBuiltinExecutor{signals: fake}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpSignalList,
		WorkspaceID: "ws-1",
		Claim:       true,
		Limit:       10,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(fake.claimCalls) != 1 {
		t.Fatalf("ClaimSignals calls = %d, want 1", len(fake.claimCalls))
	}
	if fake.claimCalls[0].workspaceID != "ws-1" || fake.claimCalls[0].limit != 10 {
		t.Errorf("claim call = %+v, unexpected", fake.claimCalls[0])
	}
	if len(fake.listCalls) != 0 {
		t.Fatalf("ListSignals should NOT be called when --claim is set, calls=%+v", fake.listCalls)
	}
	if !strings.Contains(resp.Stdout, `"Attempts": 1`) {
		t.Errorf("stdout missing attempts: %s", resp.Stdout)
	}
}

// TestBoidBuiltinExecutor_SignalList_ClaimRejectsSourceFilter pins that
// --claim + --source is refused rather than silently ignoring the filter —
// ClaimSignals (signal_store.go) has no connector parameter at all.
func TestBoidBuiltinExecutor_SignalList_ClaimRejectsSourceFilter(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpSignalList,
		WorkspaceID: "ws-1",
		Claim:       true,
		Connector:   "slack/mentions",
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection for --claim combined with --source")
	}
	if len(fake.claimCalls) != 0 || len(fake.listCalls) != 0 {
		t.Fatalf("store should not be called, claim=%+v list=%+v", fake.claimCalls, fake.listCalls)
	}
}

func TestBoidBuiltinExecutor_SignalList_ClaimRejectsNonPendingState(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpSignalList,
		WorkspaceID: "ws-1",
		Claim:       true,
		SignalState: "dead",
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection for --claim combined with a non-pending --state")
	}
	if len(fake.claimCalls) != 0 {
		t.Fatalf("store should not be called, calls=%+v", fake.claimCalls)
	}
}

func TestBoidBuiltinExecutor_SignalList_ReturnsEmptyArrayNotNull(t *testing.T) {
	fake := &fakeSignalStore{listResult: nil}
	exec := &boidBuiltinExecutor{signals: fake}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpSignalList,
		WorkspaceID: "ws-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if strings.TrimSpace(resp.Stdout) != "[]" {
		t.Fatalf("stdout = %q, want [] for a nil result", resp.Stdout)
	}
}

// --- signal_ack ---

func TestBoidBuiltinExecutor_SignalAck_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalAck, SignalIDs: []string{"sig-1"}})
	if resp.ExitCode != 1 || resp.Stderr == "" {
		t.Fatalf("expected an unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_SignalAck_RequiresWorkspaceAndIDs(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}

	if resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalAck, WorkspaceID: "ws-1"}); resp.ExitCode == 0 {
		t.Fatal("expected rejection for missing signal ids")
	}
	if resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{Op: sandbox.BoidOpSignalAck, SignalIDs: []string{"sig-1"}}); resp.ExitCode == 0 {
		t.Fatal("expected rejection for missing workspace")
	}
	if len(fake.ackCalls) != 0 {
		t.Fatalf("store should not be called, calls=%+v", fake.ackCalls)
	}
}

// TestBoidBuiltinExecutor_SignalAck_Idempotent is the executor layer of
// Q14's shim/broker/executor idempotency chain: acking the same id twice
// through THIS layer succeeds both times, with the fake store's own
// idempotent AckSignals implementation (mirroring
// orchestrator.AckSignals's real "WHERE acked_at IS NULL" semantics) proving
// the executor adds no extra "already acked" rejection of its own.
func TestBoidBuiltinExecutor_SignalAck_Idempotent(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}
	req := &sandbox.BoidRequest{Op: sandbox.BoidOpSignalAck, WorkspaceID: "ws-1", SignalIDs: []string{"sig-1"}}

	first := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, req)
	if first.ExitCode != 0 {
		t.Fatalf("first ack: exit code = %d, want 0 (stderr=%q)", first.ExitCode, first.Stderr)
	}
	second := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, req)
	if second.ExitCode != 0 {
		t.Fatalf("second ack of the same id: exit code = %d, want 0 (stderr=%q)", second.ExitCode, second.Stderr)
	}
	if len(fake.ackCalls) != 2 {
		t.Fatalf("AckSignals calls = %d, want 2", len(fake.ackCalls))
	}
	if !fake.ackedIDs["sig-1"] {
		t.Fatal("sig-1 should be acked")
	}
}

// --- signal_ingest ---

func TestBoidBuiltinExecutor_SignalIngest_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op: sandbox.BoidOpSignalIngest, WorkspaceID: "ws-1", Service: "svc", Connector: "pack/conn",
		IngestPayload: []byte(`{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"x"}`),
	})
	if resp.ExitCode != 1 || resp.Stderr == "" {
		t.Fatalf("expected an unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_SignalIngest_ParsesJSONLAndForwards(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}
	payload := "" +
		`{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"jira:X-1"}` + "\n" +
		"\n" + // blank lines are skipped
		`{"id":"2","occurred_at":"2026-08-26T01:00:00Z","identity":"jira:X-2","title":"t"}` + "\n"

	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op:            sandbox.BoidOpSignalIngest,
		WorkspaceID:   "ws-1",
		Service:       "jira-api",
		Connector:     "jira-cloud/jira-cloud",
		IngestPayload: []byte(payload),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(fake.ingestCalls) != 1 {
		t.Fatalf("IngestSignals calls = %d, want 1", len(fake.ingestCalls))
	}
	call := fake.ingestCalls[0]
	if call.workspaceID != "ws-1" || call.service != "jira-api" || call.connector != "jira-cloud/jira-cloud" {
		t.Errorf("scope = %+v, unexpected", call)
	}
	if len(call.rows) != 2 || call.rows[0].ID != "1" || call.rows[1].ID != "2" {
		t.Fatalf("rows = %+v, want 2 parsed rows", call.rows)
	}
}

func TestBoidBuiltinExecutor_SignalIngest_RejectsMalformedLine(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op:            sandbox.BoidOpSignalIngest,
		WorkspaceID:   "ws-1",
		Service:       "svc",
		Connector:     "pack/conn",
		IngestPayload: []byte("not json\n"),
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection for a malformed JSONL line")
	}
	if len(fake.ingestCalls) != 0 {
		t.Fatalf("IngestSignals should not be called, calls=%+v", fake.ingestCalls)
	}
}

func TestBoidBuiltinExecutor_SignalIngest_RequiresScope(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}
	payload := []byte(`{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"x"}`)

	cases := []*sandbox.BoidRequest{
		{Op: sandbox.BoidOpSignalIngest, Service: "svc", Connector: "pack/conn", IngestPayload: payload},      // missing workspace
		{Op: sandbox.BoidOpSignalIngest, WorkspaceID: "ws-1", Connector: "pack/conn", IngestPayload: payload}, // missing service
		{Op: sandbox.BoidOpSignalIngest, WorkspaceID: "ws-1", Service: "svc", IngestPayload: payload},         // missing connector
	}
	for _, req := range cases {
		if resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, req); resp.ExitCode == 0 {
			t.Errorf("request %+v: expected rejection", req)
		}
	}
	if len(fake.ingestCalls) != 0 {
		t.Fatalf("IngestSignals should not be called, calls=%+v", fake.ingestCalls)
	}
}

// --- signal_cursor_get ---

func TestBoidBuiltinExecutor_SignalCursorGet_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op: sandbox.BoidOpSignalCursorGet, WorkspaceID: "ws-1", Service: "svc", Connector: "pack/conn",
	})
	if resp.ExitCode != 1 || resp.Stderr == "" {
		t.Fatalf("expected an unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_SignalCursorGet_ReturnsCursor(t *testing.T) {
	fake := &fakeSignalStore{cursor: "2026-08-26T00:00:00.000000000Z"}
	exec := &boidBuiltinExecutor{signals: fake}
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op: sandbox.BoidOpSignalCursorGet, WorkspaceID: "ws-1", Service: "jira-api", Connector: "jira-cloud/jira-cloud",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if fake.cursorArg.workspaceID != "ws-1" || fake.cursorArg.service != "jira-api" || fake.cursorArg.connector != "jira-cloud/jira-cloud" {
		t.Errorf("cursor arg = %+v, unexpected", fake.cursorArg)
	}
	if !strings.Contains(resp.Stdout, `"cursor": "2026-08-26T00:00:00.000000000Z"`) {
		t.Errorf("stdout missing cursor: %s", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_SignalCursorGet_RequiresScope(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}
	cases := []*sandbox.BoidRequest{
		{Op: sandbox.BoidOpSignalCursorGet, Service: "svc", Connector: "pack/conn"},
		{Op: sandbox.BoidOpSignalCursorGet, WorkspaceID: "ws-1", Connector: "pack/conn"},
		{Op: sandbox.BoidOpSignalCursorGet, WorkspaceID: "ws-1", Service: "svc"},
	}
	for _, req := range cases {
		if resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, req); resp.ExitCode == 0 {
			t.Errorf("request %+v: expected rejection", req)
		}
	}
}

// --- wiring ---

// TestNewBoidBuiltinExecutor_SignalsNilWhenWorkflowDoesNotImplementIt
// confirms a WorkflowService test double that does NOT implement
// api.SignalStore's methods leaves the field nil rather than panicking — the
// same convention TestNewBoidBuiltinExecutor_ActionListNilWhenWorkflowDoesNotImplementIt
// pins for actionList. As of PR-3, no production wiring value implements
// api.SignalStore yet (see boidBuiltinExecutor.signals's own doc comment) —
// that wiring lands in a later PR, so there is deliberately no
// "production wire actually satisfies it" counterpart test here yet.
func TestNewBoidBuiltinExecutor_SignalsNilWhenWorkflowDoesNotImplementIt(t *testing.T) {
	got := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil)
	exec, ok := got.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("newBoidBuiltinExecutor returned %T, want *boidBuiltinExecutor", got)
	}
	if exec.signals != nil {
		t.Fatal("signals should stay nil for a workflow double that doesn't implement api.SignalStore")
	}
}

// signalWorkflowDouble implements both api.WorkflowService (via
// *recordingWorkflow embedding) and api.SignalStore (via *fakeSignalStore
// embedding), so newBoidBuiltinExecutor's runtime interface check picks up
// the SAME value's SignalStore methods — mirroring how actionListService's
// own wiring test exercises the "does get picked up" half of the check.
type signalWorkflowDouble struct {
	*recordingWorkflow
	*fakeSignalStore
}

func TestNewBoidBuiltinExecutor_WiresSignalsFromWorkflow(t *testing.T) {
	workflow := &signalWorkflowDouble{recordingWorkflow: &recordingWorkflow{}, fakeSignalStore: &fakeSignalStore{}}
	got := newBoidBuiltinExecutor(workflow, nil, nil, nil, nil, "", nil)
	exec, ok := got.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("newBoidBuiltinExecutor returned %T, want *boidBuiltinExecutor", got)
	}
	if exec.signals == nil {
		t.Fatal("signals was not wired from a workflow value that implements api.SignalStore")
	}
}
