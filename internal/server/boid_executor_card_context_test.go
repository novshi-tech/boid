package server

// Tests for boid_executor.go's BoidOpCardContext case. Broker-side scoping
// (rejecting a job with no card context before it ever reaches the
// executor) is covered separately in internal/sandbox's
// TestBroker_BoidCardContext*; these pin the executor's own logic: the live
// card_requests lookup, origin derivation from cause_id, the card_id
// cross-check, --field extraction, and the "unavailable" guard when no
// cardRequestReader is wired.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type fakeCardRequestReader struct {
	rows map[string]*orchestrator.CardRequest
	err  error
	// attachErr, when set, is returned by AttachCardRequest instead of
	// mutating rows — lets a test simulate a concurrent-attach race.
	attachErr error
	// winnerAfterAttachErr, when set alongside attachErr, is what
	// GetCardRequest returns for AFTER AttachCardRequest has failed once —
	// simulating a concurrent caller's attach having won the race in the
	// meantime.
	winnerAfterAttachErr *orchestrator.CardRequest
	attachFailed         bool
	// reclaimAtAttach, when non-empty, is the launcher_job_id
	// AttachCardRequestOwned compares expectedLauncherJobID against INSTEAD
	// of the row's own LauncherJobID — simulating a force-release followed
	// by a different launcher's reclaim landing between the earlier
	// GetCardRequest read (which still sees the original launcher) and this
	// write.
	reclaimAtAttach string
}

func (f *fakeCardRequestReader) GetCardRequest(id string) (*orchestrator.CardRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.attachFailed && f.winnerAfterAttachErr != nil {
		return f.winnerAfterAttachErr, nil
	}
	row, ok := f.rows[id]
	if !ok {
		return nil, orchestrator.ErrCardRequestNotFound
	}
	return row, nil
}

func (f *fakeCardRequestReader) AttachCardRequestOwned(id, expectedLauncherJobID, targetKind, targetID string) error {
	if f.attachErr != nil {
		f.attachFailed = true
		return f.attachErr
	}
	row, ok := f.rows[id]
	if !ok {
		return orchestrator.ErrCardRequestNotFound
	}
	currentOwner := row.LauncherJobID
	if f.reclaimAtAttach != "" {
		currentOwner = f.reclaimAtAttach
	}
	if currentOwner != expectedLauncherJobID {
		return orchestrator.ErrCardRequestOwnerMismatch
	}
	if row.Status != orchestrator.CardRequestStatusLaunching {
		return orchestrator.ErrCardRequestInvalidTransition
	}
	row.Status = orchestrator.CardRequestStatusAttached
	row.TargetKind = targetKind
	row.TargetID = targetID
	return nil
}

func TestBoidOpCardContext_NoCardContext_ClearError(t *testing.T) {
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, &fakeCardRequestReader{}, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for a job with no card context")
	}
	if !strings.Contains(resp.Stderr, "no card context") {
		t.Errorf("Stderr = %q, want a clear \"no card context\" message", resp.Stderr)
	}
}

func TestBoidOpCardContext_Unavailable_WhenNoReader(t *testing.T) {
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, nil, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero when no cardRequestReader is wired")
	}
	if !strings.Contains(resp.Stderr, "unavailable") {
		t.Errorf("Stderr = %q, want \"unavailable\"", resp.Stderr)
	}
}

func TestBoidOpCardContext_HumanOrigin_ReturnsFullContext(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {
			ID:          "req-1",
			CardID:      "card-1",
			CommandKey:  "review",
			CauseID:     "", // human-initiated: no cause id
			Instruction: "focus on the auth flow",
			Launched:    orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "python3 scripts/review.py"},
		},
	}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, Stderr = %q", resp.ExitCode, resp.Stderr)
	}
	var got cardContextResponse
	if err := json.Unmarshal([]byte(resp.Stdout), &got); err != nil {
		t.Fatalf("unmarshal response: %v (stdout=%q)", err, resp.Stdout)
	}
	want := cardContextResponse{
		CardID:      "card-1",
		RequestID:   "req-1",
		CommandKey:  "review",
		Instruction: "focus on the auth flow",
		Origin:      "human",
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestBoidOpCardContext_EventOrigin_DerivedFromCauseID(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-2": {
			ID:       "req-2",
			CardID:   "card-1",
			CauseID:  "signal-abc",
			Launched: orchestrator.CardRequestDefinition{CommandKey: "review"},
		},
	}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-2"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, Stderr = %q", resp.ExitCode, resp.Stderr)
	}
	var got cardContextResponse
	if err := json.Unmarshal([]byte(resp.Stdout), &got); err != nil {
		t.Fatalf("unmarshal response: %v (stdout=%q)", err, resp.Stdout)
	}
	if got.Origin != "event" {
		t.Errorf("Origin = %q, want %q (a non-empty cause_id means an internal event caused this request)", got.Origin, "event")
	}
}

// TestBoidOpCardContext_MismatchedCardID_Rejected pins that a token whose
// CardID disagrees with the looked-up row's own CardID (a daemon bug, since
// the broker and dispatcher both stamp both fields from the same
// token/JobSpec together) is refused rather than papered over.
func TestBoidOpCardContext_MismatchedCardID_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Launched: orchestrator.CardRequestDefinition{CommandKey: "review"}},
	}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-OTHER", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for a card_id mismatch")
	}
	if !strings.Contains(resp.Stderr, "card_id") {
		t.Errorf("Stderr = %q, want it to mention card_id", resp.Stderr)
	}
}

// TestBoidOpCardContext_EmptyCardIDWithRequestID_Rejected pins that a token
// stamped with CardRequestID but NOT CardID is refused rather than silently
// skipping the cross-check (that mis-stamping is exactly the bug class the
// check exists to catch, so it must not fail open).
func TestBoidOpCardContext_EmptyCardIDWithRequestID_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Launched: orchestrator.CardRequestDefinition{CommandKey: "review"}},
	}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero when CardID is empty")
	}
	if !strings.Contains(resp.Stderr, "card_id") {
		t.Errorf("Stderr = %q, want it to mention card_id", resp.Stderr)
	}
}

func TestBoidOpCardContext_FieldExtraction(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {
			ID:          "req-1",
			CardID:      "card-1",
			Instruction: "look at auth",
			Launched:    orchestrator.CardRequestDefinition{CommandKey: "review"},
		},
	}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpCardContext,
		TaskField: "instruction",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, Stderr = %q", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "look at auth" {
		t.Errorf("Stdout = %q, want %q", resp.Stdout, "look at auth")
	}
}

// TestBoidOpCardContext_OversizedInstruction_Rejected pins the read-side
// defense against an unbounded stored instruction: the row itself has no
// write-time size cap yet, so this op refuses to forward an oversized value
// to stdout rather than silently doing so.
func TestBoidOpCardContext_OversizedInstruction_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {
			ID:          "req-1",
			CardID:      "card-1",
			Instruction: strings.Repeat("x", sandbox.PayloadPatchMaxBytes+1),
			Launched:    orchestrator.CardRequestDefinition{CommandKey: "review"},
		},
	}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for an oversized instruction")
	}
	if !strings.Contains(resp.Stderr, "exceeds") {
		t.Errorf("Stderr = %q, want it to mention the size limit", resp.Stderr)
	}
}

func TestBoidOpCardContext_NotFound_PropagatesError(t *testing.T) {
	reader := &fakeCardRequestReader{err: errors.New("boom")}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader, nil)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-missing"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero")
	}
	if !strings.Contains(resp.Stderr, "boom") {
		t.Errorf("Stderr = %q, want it to surface the underlying error", resp.Stderr)
	}
}
