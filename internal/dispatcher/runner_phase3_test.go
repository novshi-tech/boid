package dispatcher_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/testutil"
)

// fakeSandboxPreparer intercepts the sandbox.Spec that Runner.Dispatch
// produces so tests can assert on its contents without actually running bash.
type fakeSandboxPreparer struct {
	outerPaths []string
	calls      []sandbox.Spec
	err        error
}

func (p *fakeSandboxPreparer) PrepareSandbox(spec sandbox.Spec) (*dispatcher.PreparedSandbox, error) {
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

func TestRunnerDispatch_ForwardsFieldsToSandboxSpec(t *testing.T) {
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

	request := &orchestrator.DispatchRequest{
		TaskID:     "task-phase3-12345678",
		ProjectID:  "proj-1",
		HandlerID:  "hook-a",
		Role:       orchestrator.RoleHook,
		ProjectDir: projectDir,
		HomeDir:    "/home/tester",
		HookFiles: []orchestrator.HookFile{
			{Source: projectDir + "/.boid/hooks/hook-a.sh", TargetName: "hook-a.sh"},
		},
		HookScript:   "hook-a.sh",
		BoidBinary:   "/bin/true",
		ServerSocket: "/tmp/boid.sock",
		Env: map[string]string{
			"FOO": "bar",
		},
		HostCommands: map[string]orchestrator.CommandDef{
			"git":  {Name: "git"},
			"boid": {Name: "boid"},
		},
		AdditionalBindings: []orchestrator.BindMount{
			{Source: "/opt/tools", Mode: "ro"},
		},
		WorkspaceDirs: map[string]string{"peer": "/workspace/peer"},
		ProxyPort:     9090,
		StagingDir:    "/tmp/staging",
		WorktreeDir:   worktreeDir,
		PayloadJSON:   `{"ok":true}`,
		Readonly:      true,
	}

	jobID, err := runner.Dispatch(context.Background(), &dispatcher.DispatchPlan{
		Request:            request,
		TaskID:             request.TaskID,
		ProjectID:          request.ProjectID,
		HandlerID:          request.HandlerID,
		Role:               string(request.Role),
		ProjectDir:         request.ProjectDir,
		HomeDir:            request.HomeDir,
		HookFiles:          []orchestrator.HookFile{{Source: request.HookFiles[0].Source, TargetName: request.HookFiles[0].TargetName}},
		HookScript:         request.HookScript,
		BoidBinary:         request.BoidBinary,
		ServerSocket:       request.ServerSocket,
		Env:                request.Env,
		HostCommands:       map[string]orchestrator.CommandDef{"git": {Name: "git"}, "boid": {Name: "boid"}},
		AdditionalBindings: []orchestrator.BindMount{{Source: "/opt/tools", Mode: "ro"}},
		WorkspaceDirs:      request.WorkspaceDirs,
		ProxyPort:          request.ProxyPort,
		StagingDir:         request.StagingDir,
		WorktreeDir:        request.WorktreeDir,
		PayloadJSON:        request.PayloadJSON,
		Readonly:           request.Readonly,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(preparer.calls) != 1 {
		t.Fatalf("PrepareSandbox calls = %d, want 1", len(preparer.calls))
	}

	got := preparer.calls[0]
	if got.ID != jobID {
		t.Fatalf("sandbox spec ID = %q, want %q", got.ID, jobID)
	}
	if got.WorkDir != worktreeDir {
		t.Fatalf("sandbox spec workDir = %q, want %q", got.WorkDir, worktreeDir)
	}
	// Hook role → stdin is the payload JSON, argv points at the hook script inside the worktree.
	if string(got.StdinBytes) != `{"ok":true}` {
		t.Fatalf("sandbox spec stdin = %q", string(got.StdinBytes))
	}
	wantArgv := []string{worktreeDir + "/.boid/hooks/hook-a.sh"}
	if !reflect.DeepEqual(got.Argv, wantArgv) {
		t.Fatalf("sandbox spec argv = %v, want %v", got.Argv, wantArgv)
	}
	// Broker socket shows up as an explicit Mount; token+socket env exported.
	if got.Env["BOID_BROKER_TOKEN"] != "token-phase3" {
		t.Fatalf("broker token env = %q", got.Env["BOID_BROKER_TOKEN"])
	}
	if got.Env["BOID_BROKER_SOCKET"] != "/run/boid/broker.sock" {
		t.Fatalf("broker socket env = %q", got.Env["BOID_BROKER_SOCKET"])
	}
	foundBrokerMount := false
	for _, m := range got.Mounts {
		if m.Target == "/run/boid/broker.sock" && m.Source == "/tmp/fake-broker.sock" && m.IsFile {
			foundBrokerMount = true
		}
	}
	if !foundBrokerMount {
		t.Errorf("broker socket mount missing from spec.Mounts: %+v", got.Mounts)
	}
	if !got.TTY {
		t.Errorf("hook role should set TTY=true, got false")
	}
}

func TestWriteExecScripts_BuildsSandboxSpec(t *testing.T) {
	preparer := &fakeSandboxPreparer{
		outerPaths: []string{"/tmp/boid-exec.sh"},
	}

	outerPath, err := dispatcher.WriteExecScripts(dispatcher.ExecRequest{
		JobID:        "exec-proj-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/workspace/proj-1",
		HomeDir:      "/home/tester",
		Argv:         []string{"git", "status"},
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
	if got.ID != "exec-proj-1" {
		t.Fatalf("sandbox spec ID = %q", got.ID)
	}
	if got.WorkDir != "/workspace/proj-1" {
		t.Fatalf("sandbox spec workDir = %q", got.WorkDir)
	}
	if !reflect.DeepEqual(got.Argv, []string{"git", "status"}) {
		t.Fatalf("sandbox spec argv = %v", got.Argv)
	}
	if got.Env["FOO"] != "bar" {
		t.Errorf("env[FOO] = %q, want bar", got.Env["FOO"])
	}
	if got.Env["BOID_BROKER_TOKEN"] != "token-exec" {
		t.Errorf("env[BOID_BROKER_TOKEN] = %q", got.Env["BOID_BROKER_TOKEN"])
	}
	if got.Env["BOID_JOB_ID"] != "exec-proj-1" {
		t.Errorf("env[BOID_JOB_ID] = %q", got.Env["BOID_JOB_ID"])
	}
	if got.ProxyPort != 3128 {
		t.Errorf("proxy port = %d", got.ProxyPort)
	}
	if !got.TTY {
		t.Errorf("TTY should be true")
	}
}
