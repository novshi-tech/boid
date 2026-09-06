package dispatcher

// Pins that Runner.Dispatch's tokenCtx construction copies
// spec.CardID/CardRequestID into sandbox.TokenContext.CardID/CardRequestID.
// Mirrors TestDispatch_SignalServiceConnector_ThreadedIntoTokenContext.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestDispatch_CardContext_ThreadedIntoTokenContext(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	broker := &recordingBroker{}
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{},
		BoidBinary: "/boid",
		Broker:     broker,
	}

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindExec,
		Visibility: orchestrator.Visibility{Writable: false},
		BuiltinPolicies: map[string]orchestrator.BuiltinPolicy{
			"boid": {AllowedOps: []string{orchestrator.OpBoidCardContext}},
		},
		CardID:        "card-1",
		CardRequestID: "req-1",
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if broker.calls != 1 {
		t.Fatalf("RegisterCommands calls = %d, want 1", broker.calls)
	}
	if broker.lastCtx.CardID != "card-1" {
		t.Errorf("TokenContext.CardID = %q, want %q", broker.lastCtx.CardID, "card-1")
	}
	if broker.lastCtx.CardRequestID != "req-1" {
		t.Errorf("TokenContext.CardRequestID = %q, want %q", broker.lastCtx.CardRequestID, "req-1")
	}
}

// TestDispatch_NoCardContext_TokenContextStaysEmpty pins the unchanged
// default: every ordinary job (CardID/CardRequestID left at their zero
// value — every job today) gets an empty TokenContext.CardID/CardRequestID,
// so `boid card context` stays unreachable-with-a-clear-error until a real
// producer sets them.
func TestDispatch_NoCardContext_TokenContextStaysEmpty(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	broker := &recordingBroker{}
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{},
		BoidBinary: "/boid",
		Broker:     broker,
	}

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{Writable: true},
		BuiltinPolicies: map[string]orchestrator.BuiltinPolicy{
			"boid": {AllowedOps: []string{orchestrator.OpBoidTaskCreate}},
		},
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if broker.lastCtx.CardID != "" || broker.lastCtx.CardRequestID != "" {
		t.Errorf("TokenContext.CardID/CardRequestID = %q/%q, want empty", broker.lastCtx.CardID, broker.lastCtx.CardRequestID)
	}
}
