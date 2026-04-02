package dispatcher_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

type fakeSandboxPreparer struct {
	outerPaths []string
	calls      []dispatcher.SandboxSpec
	err        error
}

func (p *fakeSandboxPreparer) PrepareSandbox(spec dispatcher.SandboxSpec) (*dispatcher.PreparedSandbox, error) {
	p.calls = append(p.calls, spec)
	if p.err != nil {
		return nil, p.err
	}

	outerPath := fmt.Sprintf("/tmp/fake-sandbox-%d.sh", len(p.calls))
	if len(p.outerPaths) > 0 {
		outerPath = p.outerPaths[0]
		p.outerPaths = p.outerPaths[1:]
	}

	return &dispatcher.PreparedSandbox{OuterPath: outerPath}, nil
}

func TestRunnerDispatch_UsesDispatcherOwnedSandboxPreparer(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()
	worktreeDir := t.TempDir()

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        "task-phase3-12345678",
		ProjectID: "proj-1",
		Title:     "sandbox interface",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	broker := &fakeBroker{
		socketPath: "/tmp/fake-broker.sock",
		tokens:     []string{"token-phase3"},
	}
	preparer := &fakeSandboxPreparer{
		outerPaths: []string{"/tmp/boid-phase3.sh"},
	}
	runner := &dispatcher.Runner{
		DB:      db.Conn,
		Runtime: newStatefulRuntime(),
		Broker:  broker,
		Sandbox: preparer,
	}

	jobID, err := runner.Dispatch(context.Background(), &dispatcher.DispatchPlan{
		TaskID:       "task-phase3-12345678",
		ProjectID:    "proj-1",
		HandlerID:    "hook-a",
		Role:         "hook",
		ProjectDir:   projectDir,
		HomeDir:      "/home/tester",
		HooksDir:     projectDir + "/.boid/hooks",
		HookScript:   "hook-a.sh",
		BoidBinary:   "/bin/true",
		ServerSocket: "/tmp/boid.sock",
		Env: map[string]string{
			"FOO": "bar",
		},
		HostCommands: map[string]dispatcher.CommandDef{
			"git":  {Name: "git"},
			"boid": {Name: "boid"},
		},
		AdditionalBindings: []dispatcher.BindMount{
			{Source: "/opt/tools", Mode: "ro"},
		},
		WorkspaceDirs: map[string]string{
			"peer": "/workspace/peer",
		},
		ProxyPort:   9090,
		StagingDir:  "/tmp/staging",
		WorktreeDir: worktreeDir,
		PayloadJSON: `{"ok":true}`,
		Readonly:    true,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(preparer.calls) != 1 {
		t.Fatalf("PrepareSandbox calls = %d, want 1", len(preparer.calls))
	}

	got := preparer.calls[0]
	if got.JobID != jobID {
		t.Fatalf("sandbox spec job id = %q, want %q", got.JobID, jobID)
	}
	if got.TaskID != "task-phase3-12345678" {
		t.Fatalf("sandbox spec task id = %q", got.TaskID)
	}
	if got.ProjectID != "proj-1" {
		t.Fatalf("sandbox spec project id = %q", got.ProjectID)
	}
	if got.ProjectDir != projectDir {
		t.Fatalf("sandbox spec project dir = %q, want %q", got.ProjectDir, projectDir)
	}
	if got.HomeDir != "/home/tester" {
		t.Fatalf("sandbox spec home dir = %q", got.HomeDir)
	}
	if got.HooksDir != projectDir+"/.boid/hooks" {
		t.Fatalf("sandbox spec hooks dir = %q", got.HooksDir)
	}
	if got.HookScript != "hook-a.sh" {
		t.Fatalf("sandbox spec hook script = %q", got.HookScript)
	}
	if got.BoidBinary != "/bin/true" {
		t.Fatalf("sandbox spec boid binary = %q", got.BoidBinary)
	}
	if got.ServerSocket != "/tmp/boid.sock" {
		t.Fatalf("sandbox spec server socket = %q", got.ServerSocket)
	}
	if got.BrokerSocket != "/tmp/fake-broker.sock" {
		t.Fatalf("sandbox spec broker socket = %q", got.BrokerSocket)
	}
	if got.BrokerToken != "token-phase3" {
		t.Fatalf("sandbox spec broker token = %q", got.BrokerToken)
	}
	if got.Role != "hook" {
		t.Fatalf("sandbox spec role = %q", got.Role)
	}
	if got.StagingDir != "/tmp/staging" {
		t.Fatalf("sandbox spec staging dir = %q", got.StagingDir)
	}
	if got.WorktreeDir != worktreeDir {
		t.Fatalf("sandbox spec worktree dir = %q", got.WorktreeDir)
	}
	if got.PayloadJSON != `{"ok":true}` {
		t.Fatalf("sandbox spec payload = %q", got.PayloadJSON)
	}
	if !got.Readonly {
		t.Fatalf("sandbox spec readonly = false, want true")
	}
	if !reflect.DeepEqual(got.Env, map[string]string{"FOO": "bar"}) {
		t.Fatalf("sandbox spec env = %#v", got.Env)
	}
	if !reflect.DeepEqual(got.AdditionalBindings, []dispatcher.BindMount{{Source: "/opt/tools", Mode: "ro"}}) {
		t.Fatalf("sandbox spec additional bindings = %#v", got.AdditionalBindings)
	}
	if !reflect.DeepEqual(got.WorkspaceDirs, map[string]string{"peer": "/workspace/peer"}) {
		t.Fatalf("sandbox spec workspace dirs = %#v", got.WorkspaceDirs)
	}

	hostCommands := append([]string(nil), got.HostCommands...)
	sort.Strings(hostCommands)
	if !reflect.DeepEqual(hostCommands, []string{"boid", "git"}) {
		t.Fatalf("sandbox spec host commands = %v, want [boid git]", hostCommands)
	}
}

func TestWriteExecScripts_UsesSandboxPreparer(t *testing.T) {
	preparer := &fakeSandboxPreparer{
		outerPaths: []string{"/tmp/boid-exec.sh"},
	}

	outerPath, err := dispatcher.WriteExecScripts(dispatcher.ExecRequest{
		JobID:        "exec-proj-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/workspace/proj-1",
		HomeDir:      "/home/tester",
		Command:      "git status",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/tmp/boid.sock",
		BrokerSocket: "/tmp/broker.sock",
		BrokerToken:  "token-exec",
		Env: map[string]string{
			"FOO": "bar",
		},
		HostCommands: map[string]dispatcher.ExecCommandDef{
			"git": {},
		},
		AdditionalBindings: []dispatcher.ExecBindMount{
			{Source: "/opt/tools", Mode: "rw"},
		},
		WorkspaceDirs: map[string]string{
			"peer": "/workspace/peer",
		},
		ProxyPort: 3128,
		TTY:       true,
	}, preparer)
	if err != nil {
		t.Fatalf("WriteExecScripts: %v", err)
	}
	if outerPath != "/tmp/boid-exec.sh" {
		t.Fatalf("outer path = %q, want %q", outerPath, "/tmp/boid-exec.sh")
	}
	if len(preparer.calls) != 1 {
		t.Fatalf("PrepareSandbox calls = %d, want 1", len(preparer.calls))
	}

	got := preparer.calls[0]
	if got.JobID != "exec-proj-1" {
		t.Fatalf("sandbox spec job id = %q", got.JobID)
	}
	if got.ProjectID != "proj-1" {
		t.Fatalf("sandbox spec project id = %q", got.ProjectID)
	}
	if got.ProjectDir != "/workspace/proj-1" {
		t.Fatalf("sandbox spec project dir = %q", got.ProjectDir)
	}
	if got.Command != "git status" {
		t.Fatalf("sandbox spec command = %q", got.Command)
	}
	if got.BoidBinary != "/usr/local/bin/boid" {
		t.Fatalf("sandbox spec boid binary = %q", got.BoidBinary)
	}
	if got.ServerSocket != "/tmp/boid.sock" {
		t.Fatalf("sandbox spec server socket = %q", got.ServerSocket)
	}
	if got.BrokerSocket != "/tmp/broker.sock" {
		t.Fatalf("sandbox spec broker socket = %q", got.BrokerSocket)
	}
	if got.BrokerToken != "token-exec" {
		t.Fatalf("sandbox spec broker token = %q", got.BrokerToken)
	}
	if got.ProxyPort != 3128 {
		t.Fatalf("sandbox spec proxy port = %d", got.ProxyPort)
	}
	if !got.TTY {
		t.Fatalf("sandbox spec tty = false, want true")
	}
	if !reflect.DeepEqual(got.AdditionalBindings, []dispatcher.BindMount{{Source: "/opt/tools", Mode: "rw"}}) {
		t.Fatalf("sandbox spec additional bindings = %#v", got.AdditionalBindings)
	}
	if !reflect.DeepEqual(got.WorkspaceDirs, map[string]string{"peer": "/workspace/peer"}) {
		t.Fatalf("sandbox spec workspace dirs = %#v", got.WorkspaceDirs)
	}

	if !reflect.DeepEqual(got.HostCommands, []string{"git"}) {
		t.Fatalf("sandbox spec host commands = %v, want [git]", got.HostCommands)
	}
}
