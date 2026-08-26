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

	"github.com/novshi-tech/boid/internal/api"
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
	// signalsJSONResponse renders the apiwire.ListSignalsResponse envelope
	// (M3, PR #1014 review) — same lower-case field names as PR-2's host
	// `GET /api/signals`, wrapped in a top-level "signals" array.
	if !strings.Contains(resp.Stdout, `"signals": [`) || !strings.Contains(resp.Stdout, `"id": "sig-1"`) {
		t.Errorf("stdout missing envelope-wrapped signal id: %s", resp.Stdout)
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
	if !strings.Contains(resp.Stdout, `"attempts": 1`) {
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
	if !strings.Contains(resp.Stdout, `"signals": []`) {
		t.Fatalf("stdout = %q, want an empty (never null) signals array in the envelope", resp.Stdout)
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

// TestBoidBuiltinExecutor_SignalIngest_EmptyPayloadIsNoOpSuccess pins m2
// (review of PR #1014, 2026-08-26): a connector that polls its source and
// finds nothing new — the ordinary outcome of most polling cycles — must
// not get a non-zero exit for calling `boid signal ingest` with an empty
// (or blank-lines-only) body, matching orchestrator.IngestSignals' own
// no-op-success contract for an empty rows slice.
func TestBoidBuiltinExecutor_SignalIngest_EmptyPayloadIsNoOpSuccess(t *testing.T) {
	fake := &fakeSignalStore{}
	exec := &boidBuiltinExecutor{signals: fake}

	cases := map[string][]byte{
		"zero bytes":       []byte(""),
		"blank lines only": []byte("\n\n   \n"),
	}
	for name, payload := range cases {
		resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
			Op:            sandbox.BoidOpSignalIngest,
			WorkspaceID:   "ws-1",
			Service:       "svc",
			Connector:     "pack/conn",
			IngestPayload: payload,
		})
		if resp.ExitCode != 0 {
			t.Errorf("%s: exit code = %d, want 0 (stderr=%q)", name, resp.ExitCode, resp.Stderr)
		}
	}
	if len(fake.ingestCalls) != len(cases) {
		t.Fatalf("IngestSignals calls = %d, want %d (one per case, forwarded as a no-op)", len(fake.ingestCalls), len(cases))
	}
	for _, call := range fake.ingestCalls {
		if len(call.rows) != 0 {
			t.Errorf("rows = %+v, want empty", call.rows)
		}
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
//
// [M1, review of PR #1014, 2026-08-26] `signals` used to be populated via a
// runtime-interface-check against `workflow`, the same pattern
// resolveOrCapture/actionList use below it in this file — but no concrete
// type ever passed as `workflow` (production wire.go's
// *api.TaskWorkflowService included) implements api.SignalStore's methods,
// so that check always silently resolved to nil in production: every signal
// op replied "unavailable" for real callers while every unit test still
// passed (the tests built their own double that DID implement the
// interface, which is exactly the class of gap
// TestNewBoidBuiltinExecutor_WiresActionListFromWorkflow's own doc comment
// warns about — "テストが具象型を直接渡し本番の wire だけアサーションを通
// る構図"). `signals` is now an explicit constructor parameter instead (see
// wire.go's call site, which passes `taskRepo` directly), so the wiring
// tests below exercise the ACTUAL parameter-passing, not a type assertion.

// TestNewBoidBuiltinExecutor_SignalsNilWhenNotProvided confirms a nil
// `signals` argument leaves the field nil rather than panicking — the "no
// store provided" case (e.g. a caller that doesn't have PR-3's op set
// wired up yet).
func TestNewBoidBuiltinExecutor_SignalsNilWhenNotProvided(t *testing.T) {
	got := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil)
	exec, ok := got.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("newBoidBuiltinExecutor returned %T, want *boidBuiltinExecutor", got)
	}
	if exec.signals != nil {
		t.Fatal("signals should stay nil when the caller passes nil")
	}
}

// TestNewBoidBuiltinExecutor_WiresSignalsFromRealTaskRepository is the
// "production wire actually satisfies it" positive test M1's fix requires —
// mirroring TestNewBoidBuiltinExecutor_WiresActionListFromWorkflow's own
// intent, but for an explicit parameter rather than a type assertion: it
// uses the REAL production types on both sides (*api.TaskWorkflowService as
// workflow — signal ops never call into it, so its zero value is fine — and
// a *orchestrator.TaskRepository backed by a real migrated test DB as
// signals, exactly what wire.go passes as `taskRepo`), then drives an
// actual signal_list call through ExecuteBoidBuiltin end to end to prove
// the wiring is live, not just non-nil.
func TestNewBoidBuiltinExecutor_WiresSignalsFromRealTaskRepository(t *testing.T) {
	// newBoidExecutorTestDB (boid_executor_task_identity_test.go) rather than
	// testutil.NewTestDB: this file is `package server`, and testutil imports
	// internal/server (for testutil.NewTestServer), so importing testutil
	// here would be an import cycle.
	taskRepo := orchestrator.NewTaskRepository(newBoidExecutorTestDB(t))
	workflow := &api.TaskWorkflowService{}

	got := newBoidBuiltinExecutor(workflow, nil, nil, nil, nil, "", nil, taskRepo)
	exec, ok := got.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("newBoidBuiltinExecutor returned %T, want *boidBuiltinExecutor", got)
	}
	if exec.signals == nil {
		t.Fatal("signals was not wired from the explicit constructor parameter")
	}

	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpSignalList,
		WorkspaceID: "ws-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("signal_list through the wired real store: exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, `"signals": []`) {
		t.Fatalf("stdout = %q, want an empty signals envelope for a fresh DB", resp.Stdout)
	}
}
