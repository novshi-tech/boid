package dispatcher

// docs/plans/signal-ingest-detailed-design.md §3.2/§5.2 (PR-5): pins that
// Runner.Dispatch's tokenCtx construction copies spec.SignalService/
// SignalConnector into sandbox.TokenContext.Service/Connector — the ONLY
// caller that ever populates these two fields (see TokenContext's own doc
// comment, internal/sandbox/protocol.go). This is a distinct concern from
// the API gateway service-allowlist override (apigateway_wire_test.go): the
// broker's builtin-op token, not the API gateway's HTTP proxy token.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// recordingBroker is a minimal dispatcher.CommandBroker fake that records
// every RegisterCommands call's TokenContext and builtin policy map.
type recordingBroker struct {
	lastCtx      sandbox.TokenContext
	lastPolicies map[string]sandbox.BuiltinPolicy
	calls        int
}

func (b *recordingBroker) RegisterCommands(_ map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx sandbox.TokenContext, _ SecretResolver) string {
	b.calls++
	b.lastCtx = ctx
	b.lastPolicies = builtinPolicies
	return "broker-token"
}

func (b *recordingBroker) SocketPath() string { return "/tmp/broker.sock" }

func (b *recordingBroker) UnregisterCommandToken(string) {}

func TestDispatch_SignalServiceConnector_ThreadedIntoTokenContext(t *testing.T) {
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
			"boid": {AllowedOps: []string{orchestrator.OpBoidSignalIngest, orchestrator.OpBoidSignalCursorGet}},
		},
		SignalService:   "slack-api",
		SignalConnector: "slack/mentions",
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if broker.calls != 1 {
		t.Fatalf("RegisterCommands calls = %d, want 1", broker.calls)
	}
	if broker.lastCtx.Service != "slack-api" {
		t.Errorf("TokenContext.Service = %q, want %q", broker.lastCtx.Service, "slack-api")
	}
	if broker.lastCtx.Connector != "slack/mentions" {
		t.Errorf("TokenContext.Connector = %q, want %q", broker.lastCtx.Connector, "slack/mentions")
	}
}

// TestDispatch_NoSignalFields_TokenContextServiceConnectorStayEmpty pins the
// unchanged default: every ordinary job (SignalService/SignalConnector left
// at their zero value) gets an empty TokenContext.Service/Connector, exactly
// as PR-3 left it — signal_ingest/signal_cursor_get stay unreachable for a
// job that doesn't explicitly opt in.
func TestDispatch_NoSignalFields_TokenContextServiceConnectorStayEmpty(t *testing.T) {
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
	if broker.lastCtx.Service != "" || broker.lastCtx.Connector != "" {
		t.Errorf("TokenContext.Service/Connector = %q/%q, want empty", broker.lastCtx.Service, broker.lastCtx.Connector)
	}
}
