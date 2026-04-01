package dispatcher_test

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

type fakeBroker struct {
	socketPath   string
	tokens       []string
	registers    []fakeBrokerRegistration
	unregistered []string
}

type fakeBrokerRegistration struct {
	commands map[string]dispatcher.CommandDef
	ctx      dispatcher.BrokerContext
}

func (b *fakeBroker) RegisterCommands(commands map[string]dispatcher.CommandDef, ctx dispatcher.BrokerContext, resolve dispatcher.SecretResolver) string {
	token := "token-1"
	if len(b.tokens) > 0 {
		token = b.tokens[0]
		b.tokens = b.tokens[1:]
	}
	b.registers = append(b.registers, fakeBrokerRegistration{
		commands: commands,
		ctx:      ctx,
	})
	return token
}

func (b *fakeBroker) UnregisterCommandToken(token string) {
	b.unregistered = append(b.unregistered, token)
}

func (b *fakeBroker) SocketPath() string {
	return b.socketPath
}

func TestRunnerDispatch_UsesDispatcherOwnedBrokerInterface(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        "task-phase2-12345678",
		ProjectID: "proj-1",
		Title:     "broker interface",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	broker := &fakeBroker{
		socketPath: "/tmp/fake-broker.sock",
		tokens:     []string{"token-phase2"},
	}
	runner := &dispatcher.Runner{
		DB:          db.Conn,
		Tmux:        testutil.NewMockTmux(),
		TmuxSession: "boid",
		Broker:      broker,
		Sandbox: &fakeSandboxPreparer{
			outerPaths: []string{"/tmp/boid-phase2.sh"},
		},
	}

	jobID, err := runner.Dispatch(context.Background(), &dispatcher.DispatchPlan{
		TaskID:      "task-phase2-12345678",
		ProjectID:   "proj-1",
		HandlerID:   "hook-a",
		Role:        "hook",
		ProjectDir:  projectDir,
		HomeDir:     projectDir,
		HookScript:  "hook-a.sh",
		BoidBinary:  "/bin/true",
		PayloadJSON: `{}`,
		HostCommands: map[string]dispatcher.CommandDef{
			"git": {Name: "git"},
		},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(broker.registers) != 1 {
		t.Fatalf("RegisterCommands calls = %d, want 1", len(broker.registers))
	}
	if broker.registers[0].ctx.JobID != jobID {
		t.Fatalf("registered job id = %q, want %q", broker.registers[0].ctx.JobID, jobID)
	}
	if broker.registers[0].ctx.TaskID != "task-phase2-12345678" {
		t.Fatalf("registered task id = %q", broker.registers[0].ctx.TaskID)
	}
	if broker.registers[0].ctx.Role != "hook" {
		t.Fatalf("registered role = %q", broker.registers[0].ctx.Role)
	}

	runner.UnregisterJob(jobID)
	if len(broker.unregistered) != 1 || broker.unregistered[0] != "token-phase2" {
		t.Fatalf("unregistered tokens = %v, want [token-phase2]", broker.unregistered)
	}
}
