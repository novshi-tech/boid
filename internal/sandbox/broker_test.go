package sandbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

var testCtx = sandbox.TokenContext{
	JobID:      "job-1",
	TaskID:     "task-1",
	ProjectID:  "proj-1",
	Role:       string(projectspec.RoleHook),
	ProjectDir: "/workspace/proj-1",
}

type fakeBoidExecutor struct {
	calls []sandbox.BoidRequest
	resp  *sandbox.ExecResponse
}

func (f *fakeBoidExecutor) ExecuteBoidBuiltin(_ sandbox.TokenContext, req *sandbox.BoidRequest) *sandbox.ExecResponse {
	if req != nil {
		f.calls = append(f.calls, *req)
	}
	if f.resp != nil {
		return f.resp
	}
	return &sandbox.ExecResponse{ExitCode: 0}
}

func TestBroker_ExecCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	broker := &sandbox.Broker{SocketPath: sockPath}

	token := broker.Register(map[string]sandbox.CommandDef{
		"echo": {
			Name:            "echo",
			Path:            "/bin/echo",
			AllowedPatterns: []string{"*"},
		},
	}, nil, testCtx)
	defer broker.Unregister(token)

	if err := broker.Start(ctx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	defer broker.Stop()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	defer conn.Close()

	req := sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello", "world"},
		Token:   token,
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp sandbox.ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0; stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "hello world\n" {
		t.Errorf("stdout = %q, want %q", resp.Stdout, "hello world\n")
	}
}

func TestBroker_UnknownCommand(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo"},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "rm",
		Args:    []string{"-rf", "/"},
		Token:   token,
	})
	if resp.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", resp.ExitCode)
	}
	if resp.Stderr == "" {
		t.Error("expected non-empty stderr for unknown command")
	}
}

func TestBroker_InvalidToken(t *testing.T) {
	broker := &sandbox.Broker{}
	broker.Register(map[string]sandbox.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   "bad-token",
	})
	if resp.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", resp.ExitCode)
	}
	if resp.Stderr == "" {
		t.Error("expected non-empty stderr for invalid token")
	}
}

func TestBroker_EmptyToken(t *testing.T) {
	broker := &sandbox.Broker{}
	broker.Register(map[string]sandbox.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
	})
	if resp.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", resp.ExitCode)
	}
}

func TestBroker_Unregister(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	}, nil, testCtx)

	// Before unregister: should work
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Errorf("before unregister: exit code = %d, want 0; stderr: %s", resp.ExitCode, resp.Stderr)
	}

	broker.Unregister(token)

	// After unregister: should fail
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if resp.ExitCode != 1 {
		t.Errorf("after unregister: exit code = %d, want 1", resp.ExitCode)
	}
}

func TestBroker_PerCommandEnv(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"TEST_VAR": "hello123"},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "env",
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "TEST_VAR=hello123") {
		t.Errorf("expected TEST_VAR=hello123 in output, got:\n%s", resp.Stdout)
	}
}

func TestBroker_GitFallsBackToHostCommandWhenBuiltinNotAllowed(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"git": {
			Name:               "git",
			Path:               "/bin/echo",
			AllowedSubcommands: []string{"push"},
		},
	}, nil, sandbox.TokenContext{
		JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1", Role: string(projectspec.RoleGate),
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Args:    []string{"push", "origin", "HEAD"},
		Token:   token,
		Git:     &sandbox.GitRequest{Op: sandbox.GitOpPush, Remote: "origin"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "push origin HEAD\n" {
		t.Fatalf("stdout = %q, want %q", resp.Stdout, "push origin HEAD\n")
	}
}

func TestBroker_SecretResolution(t *testing.T) {
	broker := &sandbox.Broker{}
	resolver := func(key string) (string, error) {
		if key == "github/pat" {
			return "ghp_secret123", nil
		}
		return "", fmt.Errorf("not found: %s", key)
	}

	token := broker.RegisterWithSecrets(map[string]sandbox.CommandDef{
		"env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"GH_TOKEN": "secret:github/pat", "PLAIN": "value"},
		},
	}, nil, sandbox.TokenContext{
		JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1", Role: string(projectspec.RoleGate),
	}, resolver)
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "env",
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "GH_TOKEN=ghp_secret123") {
		t.Error("expected resolved secret in GH_TOKEN")
	}
	if !strings.Contains(resp.Stdout, "PLAIN=value") {
		t.Error("expected plain value in PLAIN")
	}
}

func TestBroker_SecretResolutionEmptyKey(t *testing.T) {
	broker := &sandbox.Broker{}
	resolver := func(key string) (string, error) {
		if key == "GH_TOKEN" {
			return "ghp_from_env_name", nil
		}
		return "", fmt.Errorf("not found: %s", key)
	}

	token := broker.RegisterWithSecrets(map[string]sandbox.CommandDef{
		"env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"GH_TOKEN": "secret:", "PLAIN": "value"},
		},
	}, nil, sandbox.TokenContext{
		JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1", Role: string(projectspec.RoleGate),
	}, resolver)
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "env",
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "GH_TOKEN=ghp_from_env_name") {
		t.Errorf("expected GH_TOKEN resolved from env var name, got: %s", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "PLAIN=value") {
		t.Error("expected plain value in PLAIN")
	}
}

func TestBroker_RegisterReturnsUniqueTokens(t *testing.T) {
	broker := &sandbox.Broker{}
	cmds := map[string]sandbox.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo"},
	}

	t1 := broker.Register(cmds, nil, testCtx)
	t2 := broker.Register(cmds, nil, testCtx)
	if t1 == t2 {
		t.Error("Register should return unique tokens")
	}
}

func TestBroker_GetContext(t *testing.T) {
	broker := &sandbox.Broker{}
	ctx := sandbox.TokenContext{
		JobID:             "job-42",
		TaskID:            "task-99",
		ProjectID:         "proj-7",
		WorkspaceID:       "ws-7",
		AllowedProjectIDs: []string{"proj-7", "proj-8"},
		Role:              string(projectspec.RoleGate),
		ProjectDir:        "/workspace/proj-7",
		WorktreeDir:       "/workspace/proj-7-wt",
	}

	token := broker.Register(map[string]sandbox.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo"},
	}, nil, ctx)

	got, ok := broker.GetContext(token)
	if !ok {
		t.Fatal("expected GetContext to return true for valid token")
	}
	if got.JobID != "job-42" {
		t.Errorf("JobID = %q, want %q", got.JobID, "job-42")
	}
	if got.TaskID != "task-99" {
		t.Errorf("TaskID = %q, want %q", got.TaskID, "task-99")
	}
	if got.ProjectID != "proj-7" {
		t.Errorf("ProjectID = %q, want %q", got.ProjectID, "proj-7")
	}
	if got.Role != string(projectspec.RoleGate) {
		t.Errorf("Role = %q, want %q", got.Role, string(projectspec.RoleGate))
	}
	if got.WorkspaceID != "ws-7" {
		t.Errorf("WorkspaceID = %q, want %q", got.WorkspaceID, "ws-7")
	}
	if len(got.AllowedProjectIDs) != 2 {
		t.Fatalf("AllowedProjectIDs = %#v, want 2 entries", got.AllowedProjectIDs)
	}

	_, ok = broker.GetContext("nonexistent")
	if ok {
		t.Error("expected GetContext to return false for invalid token")
	}
}

func TestBroker_BoidBuiltinPolicy_HookRole(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	hookCtx := sandbox.TokenContext{
		JobID:      "j1",
		TaskID:     "t1",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleHook),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleHook, []string{"boid"}), hookCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:       sandbox.BoidOpJobDone,
			JobID:    "j1",
			ExitCode: 0,
			Output:   "done",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].Op != sandbox.BoidOpJobDone || exec.calls[0].JobID != "j1" {
		t.Fatalf("unexpected boid request: %+v", exec.calls[0])
	}

	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:       sandbox.BoidOpTaskCreate,
			Title:    "test",
			Behavior: "dev",
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("hook task create should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBroker_BoidBuiltinPolicy_GateRole(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:      "j1",
		TaskID:     "t1",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleGate),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), gateCtx)

	// Cwd = /tmp (legacy) must still be accepted
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:       sandbox.BoidOpJobDone,
			JobID:    "j1",
			ExitCode: 0,
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("job done (cwd=/tmp) exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}

	// Cwd = projectDir must also be accepted (gate now cds to workDir tmpfs)
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:       sandbox.BoidOpTaskCreate,
			Title:    "test",
			Behavior: "dev",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("task create (cwd=projectDir) exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("executor calls = %d, want 2", len(exec.calls))
	}
	if exec.calls[1].ProjectID != "p1" {
		t.Fatalf("task create project id = %q, want current project", exec.calls[1].ProjectID)
	}
}

func TestBroker_BoidBuiltinPolicy_RespectsAllowedProjectIDs(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:             "j2",
		TaskID:            "t2",
		ProjectID:         "p1",
		WorkspaceID:       "ws-1",
		AllowedProjectIDs: []string{"p1", "p2"},
		Role:              string(projectspec.RoleGate),
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:        sandbox.BoidOpTaskCreate,
			Title:     "peer task",
			Behavior:  "dev",
			ProjectID: "p2",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("same-workspace create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].ProjectID != "p2" {
		t.Fatalf("executor calls = %+v, want one call for p2", exec.calls)
	}

	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:        sandbox.BoidOpTaskCreate,
			Title:     "cross task",
			Behavior:  "dev",
			ProjectID: "p3",
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("cross-workspace create should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should not receive cross-workspace request, calls=%d", len(exec.calls))
	}
}

// gate role は BoidOpTaskUpdate を実行できる (auto-merge script が trigger
// task の artifact.pr を書き戻すユースケース)。
func TestBroker_BoidBuiltinPolicy_GateRoleTaskUpdate(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:      "j-gate",
		TaskID:     "t-gate",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleGate),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), gateCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:      sandbox.BoidOpTaskUpdate,
			TaskID:  "task-target",
			Payload: []byte(`{"artifact":{"pr":{"merged":true}}}`),
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("gate task update exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].Op != sandbox.BoidOpTaskUpdate {
		t.Fatalf("op = %q, want %q", exec.calls[0].Op, sandbox.BoidOpTaskUpdate)
	}
	if exec.calls[0].TaskID != "task-target" {
		t.Fatalf("task id = %q, want task-target", exec.calls[0].TaskID)
	}
}

// hook role は BoidOpTaskUpdate を実行できない (agent は task の書き換えを
// 行ってはならず、書き換えは gate 経由で行う)。
func TestBroker_BoidBuiltinPolicy_HookRoleRejectsTaskUpdate(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	hookCtx := sandbox.TokenContext{
		JobID:      "j-hook",
		TaskID:     "t-hook",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleHook),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleHook, []string{"boid"}), hookCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:      sandbox.BoidOpTaskUpdate,
			TaskID:  "task-target",
			Payload: []byte(`{"foo":"bar"}`),
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("hook task update should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not receive hook task update, calls=%d", len(exec.calls))
	}
}

// BoidOpTaskUpdate で TaskID を省略するとエラーになる。
func TestBroker_BoidBuiltinTaskUpdateRequiresTaskID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:      "j-gate",
		TaskID:     "t-gate",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleGate),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), gateCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:      sandbox.BoidOpTaskUpdate,
			Payload: []byte(`{"foo":"bar"}`),
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "task id") {
		t.Fatalf("expected task id error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not receive request without task id, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidBuiltinRejectsWrongJobAndCwd(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:      "job-keep",
		TaskID:     "task-keep",
		ProjectID:  "proj-keep",
		Role:       string(projectspec.RoleHook),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleHook, []string{"boid"}), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:       sandbox.BoidOpJobDone,
			JobID:    "other-job",
			ExitCode: 0,
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current job") {
		t.Fatalf("expected job id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}

	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     t.TempDir(),
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:       sandbox.BoidOpJobDone,
			JobID:    "job-keep",
			ExitCode: 0,
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current project or worktree") {
		t.Fatalf("expected cwd rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBroker_BoidBuiltinRequiresTypedRequest(t *testing.T) {
	broker := &sandbox.Broker{}
	cwd := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleHook, []string{"boid"}), sandbox.TokenContext{
		JobID:      "job-1",
		TaskID:     "task-1",
		ProjectID:  "proj-1",
		Role:       string(projectspec.RoleHook),
		ProjectDir: cwd,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     cwd,
		Token:   token,
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "typed boid request required") {
		t.Fatalf("expected typed request rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// hook role は BoidOpTaskReopen を実行できない（状態遷移は hook から禁止）。
func TestBroker_BoidBuiltinPolicy_HookRoleRejectsReopen(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	hookCtx := sandbox.TokenContext{
		JobID:      "j-hook",
		TaskID:     "t-hook",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleHook),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleHook, []string{"boid"}), hookCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:     sandbox.BoidOpTaskReopen,
			TaskID: "target-task",
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("hook task reopen should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not receive hook reopen, calls=%d", len(exec.calls))
	}
}

// gate role は BoidOpTaskReopen を実行できる（detect-conflicts kit が done → reworking に戻すユースケース）。
func TestBroker_BoidBuiltinPolicy_GateRoleAllowsReopen(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:      "j-gate",
		TaskID:     "t-gate",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleGate),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), gateCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:     sandbox.BoidOpTaskReopen,
			TaskID: "target-task",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("gate task reopen exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].Op != sandbox.BoidOpTaskReopen {
		t.Fatalf("op = %q, want %q", exec.calls[0].Op, sandbox.BoidOpTaskReopen)
	}
	if exec.calls[0].TaskID != "target-task" {
		t.Fatalf("task id = %q, want target-task", exec.calls[0].TaskID)
	}
}

// --- task import broker tests ---

func TestBroker_BoidTaskImport_GateAllowed(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:      "j1",
		TaskID:     "t1",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleGate),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), gateCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskImport,
			ImportTasks: []json.RawMessage{
				json.RawMessage(`{"project_id":"p1","title":"t","behavior":"dev"}`),
			},
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].Op != sandbox.BoidOpTaskImport {
		t.Fatalf("op = %q, want %q", exec.calls[0].Op, sandbox.BoidOpTaskImport)
	}
}

func TestBroker_BoidTaskImport_HookRejected(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	hookCtx := sandbox.TokenContext{
		JobID:      "j1",
		TaskID:     "t1",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleHook),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleHook, []string{"boid"}), hookCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskImport,
			ImportTasks: []json.RawMessage{
				json.RawMessage(`{"project_id":"p1","title":"t"}`),
			},
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed for role hook") {
		t.Fatalf("hook task import should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskImport_DisallowedProject(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "p1",
		AllowedProjectIDs: []string{"p1"},
		Role:              string(projectspec.RoleGate),
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), gateCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskImport,
			ImportTasks: []json.RawMessage{
				json.RawMessage(`{"project_id":"p2","title":"t"}`), // p2 は許可外
			},
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "outside the current workspace") {
		t.Fatalf("disallowed project should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskImport_ProjectOverrideValidated(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "p1",
		AllowedProjectIDs: []string{"p1"},
		Role:              string(projectspec.RoleGate),
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), gateCtx)

	// ImportProjectOverride = "p2" は AllowedProjectIDs に含まれないので拒否
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskImport,
			ImportTasks: []json.RawMessage{
				json.RawMessage(`{"title":"t"}`),
			},
			ImportProjectOverride: "p2",
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "outside the current workspace") {
		t.Fatalf("project override outside workspace should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskImport_DefaultProjectFromContext(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:      "j1",
		TaskID:     "t1",
		ProjectID:  "p1",
		Role:       string(projectspec.RoleGate),
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"boid"}), gateCtx)

	// project_id 未指定 → ctx.ProjectID = "p1" がデフォルト注入される
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskImport,
			ImportTasks: []json.RawMessage{
				json.RawMessage(`{"title":"t"}`),
			},
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("default project from context should succeed, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
}
