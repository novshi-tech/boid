package server

// Tests for boid_executor.go's BoidOpAgentStart case (implemented in
// boid_executor_agent_start.go). Broker-side scoping (no-card-context
// rejection, project resolution/AllowsProject) is covered separately in
// internal/sandbox's TestBroker_BoidAgentStart*; these pin the executor's
// own logic: origin rejection, idempotent retry, conflicting-target
// rejection, the actual dispatch+attach sequence, and the concurrent-attach
// race fallback.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type fakeSessionStarter struct {
	result *api.StartSessionResult
	err    error
	// calls records every StartSessionRequest this fake received.
	calls []api.StartSessionRequest
}

func (f *fakeSessionStarter) StartSession(_ context.Context, req api.StartSessionRequest) (*api.StartSessionResult, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &api.StartSessionResult{JobID: "job-new"}, nil
}

func TestBoidOpAgentStart_NoCardContext_ClearError(t *testing.T) {
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, &fakeCardRequestReader{}, &fakeSessionStarter{})
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for a job with no card context")
	}
	if !strings.Contains(resp.Stderr, "no card context") {
		t.Errorf("Stderr = %q, want a clear \"no card context\" message", resp.Stderr)
	}
}

func TestBoidOpAgentStart_Unavailable_WhenNoDependencies(t *testing.T) {
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, nil, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero when no cardRequests/sessionStarter is wired")
	}
	if !strings.Contains(resp.Stderr, "unavailable") {
		t.Errorf("Stderr = %q, want \"unavailable\"", resp.Stderr)
	}
}

func TestBoidOpAgentStart_MismatchedCardID_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching},
	}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, &fakeSessionStarter{})
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-OTHER", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for a card_id mismatch")
	}
	if !strings.Contains(resp.Stderr, "card_id") {
		t.Errorf("Stderr = %q, want it to mention card_id", resp.Stderr)
	}
}

// TestBoidOpAgentStart_StaleLauncher_Rejected pins that a job whose token
// names this card_requests id but is NOT (or no longer is) its current
// launcher is refused — otherwise a superseded launcher whose container
// outlived a Retry could hijack the fresh attempt's slot.
func TestBoidOpAgentStart_StaleLauncher_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-stale"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for a stale launcher")
	}
	if len(starter.calls) != 0 {
		t.Errorf("session dispatch calls = %d, want 0", len(starter.calls))
	}
}

// TestBoidOpAgentStart_EmptyLauncherJobID_Rejected: ClaimQueuedCardRequests
// and the launching fast path now stamp launcher_job_id atomically with the
// launching promotion (card_request.go), so a launching row with no
// launcher of record can only mean it bypassed that invariant — reject
// rather than treat it as unclaimed (the inverse of this op's old, buggy
// behavior, which let an empty LauncherJobID through unconditionally).
func TestBoidOpAgentStart_EmptyLauncherJobID_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: ""},
	}}
	starter := &fakeSessionStarter{result: &api.StartSessionResult{JobID: "job-42"}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-whatever"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for an empty launcher_job_id")
	}
	if len(starter.calls) != 0 {
		t.Errorf("session dispatch calls = %d, want 0", len(starter.calls))
	}
}

// TestBoidOpAgentStart_PropagatesCardContextOntoSession pins that the
// created session's own StartSessionRequest carries this launcher's
// CardID/CardRequestID, so its JobSpec/token (and so a crash-recovery scan)
// can reverse-link the session back to this card_requests row.
func TestBoidOpAgentStart_PropagatesCardContextOntoSession(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
	}}
	starter := &fakeSessionStarter{result: &api.StartSessionResult{JobID: "job-42"}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, Stderr = %q", resp.ExitCode, resp.Stderr)
	}
	if len(starter.calls) != 1 {
		t.Fatalf("session dispatch calls = %d, want 1", len(starter.calls))
	}
	got := starter.calls[0]
	if got.CardID != "card-1" || got.CardRequestID != "req-1" {
		t.Errorf("StartSessionRequest CardID/CardRequestID = %q/%q, want card-1/req-1", got.CardID, got.CardRequestID)
	}
}

// TestBoidOpAgentStart_EventOrigin_Rejected pins the core rule: a request
// caused by an internal event (non-empty cause_id) must never reach a
// session, since an unattended session never terminates on its own and
// would hold the card's execution slot forever.
func TestBoidOpAgentStart_EventOrigin_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", CauseID: "signal-abc", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for an event-originated request")
	}
	if !strings.Contains(resp.Stderr, "internal event") {
		t.Errorf("Stderr = %q, want it to explain the internal-event rejection", resp.Stderr)
	}
	if len(starter.calls) != 0 {
		t.Errorf("session dispatch calls = %d, want 0 (must reject before dispatching)", len(starter.calls))
	}
}

func TestBoidOpAgentStart_InvalidHarness_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "bogus"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for an invalid harness_type")
	}
	if len(starter.calls) != 0 {
		t.Errorf("session dispatch calls = %d, want 0", len(starter.calls))
	}
}

func TestBoidOpAgentStart_NotLaunchable_Rejected(t *testing.T) {
	for _, status := range []orchestrator.CardRequestStatus{
		orchestrator.CardRequestStatusQueued,
		orchestrator.CardRequestStatusFolded,
		orchestrator.CardRequestStatusFinished,
		orchestrator.CardRequestStatusFailed,
	} {
		reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
			"req-1": {ID: "req-1", CardID: "card-1", Status: status, LauncherJobID: "job-current"},
		}}
		exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, &fakeSessionStarter{})
		resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
		if resp.ExitCode == 0 {
			t.Errorf("status=%q: ExitCode = 0, want non-zero (not launchable)", status)
		}
	}
}

// TestBoidOpAgentStart_Dispatches_AndAttaches pins the happy path: a
// launching request dispatches a session through sessionStarter and
// records the continuation via AttachCardRequest.
func TestBoidOpAgentStart_Dispatches_AndAttaches(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
	}}
	starter := &fakeSessionStarter{result: &api.StartSessionResult{JobID: "job-42"}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", ProjectID: "proj-1", JobID: "job-current"}, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpAgentStart,
		ProjectID:   "proj-1",
		HarnessType: "claude",
		Instruction: "look into it",
		Model:       "opus",
		DisplayName: "card session",
		Readonly:    true,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, Stderr = %q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, `"job_id":"job-42"`) || !strings.Contains(resp.Stdout, `"kind":"session"`) {
		t.Errorf("Stdout = %q, want the session/job_id reply shape", resp.Stdout)
	}
	if len(starter.calls) != 1 {
		t.Fatalf("session dispatch calls = %d, want 1", len(starter.calls))
	}
	got := starter.calls[0]
	if got.ProjectID != "proj-1" || got.HarnessType != "claude" || got.Instruction != "look into it" || got.Model != "opus" || got.DisplayName != "card session" || !got.Readonly {
		t.Errorf("StartSessionRequest = %+v, fields not forwarded correctly", got)
	}
	row := reader.rows["req-1"]
	if row.Status != orchestrator.CardRequestStatusAttached || row.TargetKind != orchestrator.CardRequestTargetKindSession || row.TargetID != "job-42" {
		t.Errorf("card_requests row after dispatch = %+v, want attached/session/job-42", row)
	}
}

// TestBoidOpAgentStart_IdempotentRetry_ReturnsExistingSession pins the
// retry contract: calling again for an already-attached session request
// must return the SAME job id without starting a second session.
func TestBoidOpAgentStart_IdempotentRetry_ReturnsExistingSession(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {
			ID: "req-1", CardID: "card-1",
			Status:     orchestrator.CardRequestStatusAttached,
			TargetKind: orchestrator.CardRequestTargetKindSession, TargetID: "job-original",
			LauncherJobID: "job-current",
		},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, Stderr = %q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, `"job_id":"job-original"`) {
		t.Errorf("Stdout = %q, want the ORIGINAL job id (idempotent retry)", resp.Stdout)
	}
	if len(starter.calls) != 0 {
		t.Errorf("session dispatch calls = %d, want 0 (must not start a second session)", len(starter.calls))
	}
}

// TestBoidOpAgentStart_AlreadyAttachedToTask_Rejected: a request already
// attached to a TASK continuation must not be silently re-answered as a
// session — that is a conflict, not a retry.
func TestBoidOpAgentStart_AlreadyAttachedToTask_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {
			ID: "req-1", CardID: "card-1",
			Status:     orchestrator.CardRequestStatusAttached,
			TargetKind: orchestrator.CardRequestTargetKindTask, TargetID: "task-1",
			LauncherJobID: "job-current",
		},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for a task/session target conflict")
	}
	if len(starter.calls) != 0 {
		t.Errorf("session dispatch calls = %d, want 0", len(starter.calls))
	}
}

// TestBoidOpAgentStart_ConcurrentAttachRace_ReturnsWinningTarget pins the
// race fallback: if AttachCardRequest reports the row was already attached
// by someone else by the time this call tries to record it, the reply must
// carry the WINNING continuation, not the orphaned session this call just
// started.
func TestBoidOpAgentStart_ConcurrentAttachRace_ReturnsWinningTarget(t *testing.T) {
	reader := &fakeCardRequestReader{
		rows: map[string]*orchestrator.CardRequest{
			// Still "launching" at the pre-dispatch check — the race is a
			// SECOND caller for the same request winning AttachCardRequest's
			// CAS between this call's own check and its own attach attempt.
			"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
		},
		attachErr: orchestrator.ErrCardRequestInvalidTransition,
		winnerAfterAttachErr: &orchestrator.CardRequest{
			ID: "req-1", CardID: "card-1",
			Status:     orchestrator.CardRequestStatusAttached,
			TargetKind: orchestrator.CardRequestTargetKindSession,
			TargetID:   "job-winner",
		},
	}
	starter := &fakeSessionStarter{result: &api.StartSessionResult{JobID: "job-orphan"}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, Stderr = %q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, `"job_id":"job-winner"`) {
		t.Errorf("Stdout = %q, want the WINNING job id job-winner, not this call's own orphaned dispatch", resp.Stdout)
	}
	if len(starter.calls) != 1 {
		t.Errorf("session dispatch calls = %d, want 1 (this call does dispatch — it loses the attach race afterward)", len(starter.calls))
	}
}

// TestBoidOpAgentStart_OwnershipReclaimedBeforeAttach_Rejected pins the
// TOCTOU close: the pre-dispatch ownership check (row.LauncherJobID ==
// ctx.JobID) reads the row once, but by the time this call tries to attach
// its own dispatched session, a force-release + a DIFFERENT launcher's
// reclaim may have moved LauncherJobID out from under it. The attach must
// re-assert ownership in its own write and reject rather than let the
// stale caller steal the new owner's slot.
func TestBoidOpAgentStart_OwnershipReclaimedBeforeAttach_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{
		rows: map[string]*orchestrator.CardRequest{
			"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
		},
		reclaimAtAttach: "job-new-owner",
	}
	starter := &fakeSessionStarter{result: &api.StartSessionResult{JobID: "job-orphan"}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero when ownership was reclaimed before the attach")
	}
	if !strings.Contains(resp.Stderr, "no longer own") {
		t.Errorf("Stderr = %q, want it to explain the ownership reclaim, not a generic error", resp.Stderr)
	}
	if len(starter.calls) != 1 {
		t.Errorf("session dispatch calls = %d, want 1 (this call does dispatch — it loses ownership only at the attach)", len(starter.calls))
	}
}

func TestBoidOpAgentStart_AttachFailure_SurfacesError(t *testing.T) {
	reader := &fakeCardRequestReader{
		rows: map[string]*orchestrator.CardRequest{
			"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
		},
		attachErr: errors.New("db is on fire"),
	}
	starter := &fakeSessionStarter{result: &api.StartSessionResult{JobID: "job-42"}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero when recording the association fails")
	}
	if !strings.Contains(resp.Stderr, "job-42") || !strings.Contains(resp.Stderr, "db is on fire") {
		t.Errorf("Stderr = %q, want it to mention the started job id and the underlying error", resp.Stderr)
	}
}

func TestBoidOpAgentStart_SessionDispatchFailure_Propagates(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"},
	}}
	starter := &fakeSessionStarter{err: errors.New("dispatch exploded")}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", JobID: "job-current"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero when session dispatch itself fails")
	}
	if !strings.Contains(resp.Stderr, "dispatch exploded") {
		t.Errorf("Stderr = %q, want it to surface the underlying error", resp.Stderr)
	}
}
