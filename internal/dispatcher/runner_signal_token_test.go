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
// every RegisterCommands call's TokenContext, builtin policy map, and
// resolved host commands.
type recordingBroker struct {
	lastCtx      sandbox.TokenContext
	lastPolicies map[string]sandbox.BuiltinPolicy
	lastCommands map[string]orchestrator.CommandDef
	calls        int
}

func (b *recordingBroker) RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx sandbox.TokenContext, _ SecretResolver) string {
	b.calls++
	b.lastCtx = ctx
	b.lastPolicies = builtinPolicies
	b.lastCommands = commands
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

// TestDispatch_ConnectorPolicy_EndToEnd_HostCommandsNeverReachBroker is the
// full-pipeline closure of the Q27 blocker fixed in session_job.go
// (TestBuildSessionJobSpec_ConnectorPolicyTrue_ForcesHostCommandsEmpty pins
// the JobSpec-construction half; this test drives the REAL
// BuildExecJobSpec -> Runner.Dispatch -> CommandBroker.RegisterCommands
// chain end to end): a metaproject that declares project.yaml
// host_commands (e.g. a `gh` entry backed by a real host credential) must
// NOT let a connector job's broker token reach it, even though the same
// SessionJobInput.HostCommands value is exactly what an ordinary hook/exec
// job for the SAME project would get passed. internal/sandbox/broker.go's
// Handle dispatches host commands via entry.Commands, independent of
// entry.BuiltinPolicies — this is the seam a policy-only fix cannot close.
func TestDispatch_ConnectorPolicy_EndToEnd_HostCommandsNeverReachBroker(t *testing.T) {
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

	stubSessionBaseBranch(t, "main")
	input := SessionJobInput{
		ProjectID:      "proj-1",
		ProjectWorkDir: "", // no clone declaration needed for this dispatch-level test
		Readonly:       true,
		// The metaproject's project.yaml host_commands — the SAME value an
		// ordinary trigger/hook job for this project would also receive.
		HostCommands: map[string]orchestrator.HostCommandSpec{
			"gh": {Path: "/usr/bin/gh"},
		},
		ConnectorPolicy: true,
		SignalService:   "slack-api",
		SignalConnector: "slack/mentions",
	}
	spec, err := BuildExecJobSpec(input, []string{"sh", "-c", `exec "$BOID_CONNECTOR_EXEC"`}, false)
	if err != nil {
		t.Fatalf("BuildExecJobSpec: %v", err)
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if broker.calls != 1 {
		t.Fatalf("RegisterCommands calls = %d, want 1", broker.calls)
	}
	if len(broker.lastCommands) != 0 {
		t.Fatalf("RegisterCommands received host commands = %+v, want EMPTY — a connector job must never reach the metaproject's declared host_commands (Q27 blocker)", broker.lastCommands)
	}
	if !broker.lastPolicies["boid"].Allows(string(sandbox.BoidOpSignalIngest)) {
		t.Error("connector job's boid policy should still allow signal_ingest (the fix must not also break the intended grant)")
	}
}
