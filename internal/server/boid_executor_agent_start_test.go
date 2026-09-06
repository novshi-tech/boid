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

// TestBoidOpAgentStart_EventOrigin_Rejected pins the core rule: a request
// caused by an internal event (non-empty cause_id) must never reach a
// session, since an unattended session never terminates on its own and
// would hold the card's execution slot forever.
func TestBoidOpAgentStart_EventOrigin_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", CauseID: "signal-abc", Status: orchestrator.CardRequestStatusLaunching},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
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
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "bogus"})
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
			"req-1": {ID: "req-1", CardID: "card-1", Status: status},
		}}
		exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, &fakeSessionStarter{})
		resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
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
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching},
	}}
	starter := &fakeSessionStarter{result: &api.StartSessionResult{JobID: "job-42"}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1", ProjectID: "proj-1"}, &sandbox.BoidRequest{
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
		},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
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
		},
	}}
	starter := &fakeSessionStarter{}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
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
			"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching},
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
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
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

func TestBoidOpAgentStart_AttachFailure_SurfacesError(t *testing.T) {
	reader := &fakeCardRequestReader{
		rows: map[string]*orchestrator.CardRequest{
			"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching},
		},
		attachErr: errors.New("db is on fire"),
	}
	starter := &fakeSessionStarter{result: &api.StartSessionResult{JobID: "job-42"}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero when recording the association fails")
	}
	if !strings.Contains(resp.Stderr, "job-42") || !strings.Contains(resp.Stderr, "db is on fire") {
		t.Errorf("Stderr = %q, want it to mention the started job id and the underlying error", resp.Stderr)
	}
}

func TestBoidOpAgentStart_SessionDispatchFailure_Propagates(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching},
	}}
	starter := &fakeSessionStarter{err: errors.New("dispatch exploded")}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, starter)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero when session dispatch itself fails")
	}
	if !strings.Contains(resp.Stderr, "dispatch exploded") {
		t.Errorf("Stderr = %q, want it to surface the underlying error", resp.Stderr)
	}
}
