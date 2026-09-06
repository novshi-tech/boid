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
}

func (f *fakeCardRequestReader) GetCardRequest(id string) (*orchestrator.CardRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	row, ok := f.rows[id]
	if !ok {
		return nil, orchestrator.ErrCardRequestNotFound
	}
	return row, nil
}

func TestBoidOpCardContext_NoCardContext_ClearError(t *testing.T) {
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, &fakeCardRequestReader{})
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for a job with no card context")
	}
	if !strings.Contains(resp.Stderr, "no card context") {
		t.Errorf("Stderr = %q, want a clear \"no card context\" message", resp.Stderr)
	}
}

func TestBoidOpCardContext_Unavailable_WhenNoReader(t *testing.T) {
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, nil)
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
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader)
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
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader)
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

// TestBoidOpCardContext_SelfReportedCardIDCannotForgeAnotherCard pins the
// "request ID の自己申告だけで操作権限を与えない" contract at the executor
// layer: even if ctx.CardID somehow disagreed with the looked-up row's own
// CardID (a daemon bug, since the broker and dispatcher both stamp both
// fields from the same token/JobSpec together), the op must refuse rather
// than paper over the mismatch by trusting either side blindly.
func TestBoidOpCardContext_MismatchedCardID_Rejected(t *testing.T) {
	reader := &fakeCardRequestReader{rows: map[string]*orchestrator.CardRequest{
		"req-1": {ID: "req-1", CardID: "card-1", Launched: orchestrator.CardRequestDefinition{CommandKey: "review"}},
	}}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-OTHER", CardRequestID: "req-1"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero for a card_id mismatch")
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
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader)
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

func TestBoidOpCardContext_NotFound_PropagatesError(t *testing.T) {
	reader := &fakeCardRequestReader{err: errors.New("boom")}
	exec := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil, nil, reader)
	resp := exec.ExecuteBoidBuiltin(t.Context(), sandbox.TokenContext{CardID: "card-1", CardRequestID: "req-missing"}, &sandbox.BoidRequest{Op: sandbox.BoidOpCardContext})
	if resp.ExitCode == 0 {
		t.Fatalf("ExitCode = 0, want non-zero")
	}
	if !strings.Contains(resp.Stderr, "boom") {
		t.Errorf("Stderr = %q, want it to surface the underlying error", resp.Stderr)
	}
}
