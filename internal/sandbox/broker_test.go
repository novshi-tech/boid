package sandbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Local copies of orchestrator role labels / policy builders so that the
// sandbox package stays import-free of orchestrator (layer independence).
const (
	testRoleHook = "hook"
	testRoleGate = "gate"
)

func testHookBoidPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpJobDone):   {},
			string(sandbox.BoidOpAgentStop): {},
			string(sandbox.BoidOpTaskGet):   {},
			string(sandbox.BoidOpTaskList):  {},
		}},
	}
}

func testGateBoidPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {
			AllowedOps: map[string]struct{}{
				string(sandbox.BoidOpJobDone):    {},
				string(sandbox.BoidOpJobList):    {},
				string(sandbox.BoidOpJobShow):    {},
				string(sandbox.BoidOpJobLog):     {},
				string(sandbox.BoidOpActionSend): {},
				string(sandbox.BoidOpTaskCreate): {},
				string(sandbox.BoidOpTaskUpdate): {},
				string(sandbox.BoidOpTaskImport): {},
				string(sandbox.BoidOpTaskReopen): {},
				string(sandbox.BoidOpTaskList):   {},
			},
			AllowedCwdRoots: []string{"/tmp"},
		},
	}
}

// testCtx is the shared TokenContext for broker unit tests that do not care
// about host-command cwd resolution. ProjectDir is left empty here so that
// those tests fall back to req.Cwd (i.e. whatever the shim would have sent);
// tests that exercise cwd resolution set ProjectDir/WorktreeDir to a real
// directory inline.
var testCtx = sandbox.TokenContext{
	JobID:     "job-1",
	TaskID:    "task-1",
	ProjectID: "proj-1",
	Role:      testRoleHook,
}

type fakeBoidExecutor struct {
	calls []sandbox.BoidRequest
	resp  *sandbox.ExecResponse
}

func (f *fakeBoidExecutor) ExecuteBoidBuiltin(_ context.Context, _ sandbox.TokenContext, req *sandbox.BoidRequest) *sandbox.ExecResponse {
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
		"/bin/echo": {
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
		Command: "/bin/echo",
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

// runBrokerPwd is a test helper that runs /bin/pwd through a broker with the
// given TokenContext and the cwd the shim would have reported, and returns the
// broker's chosen working directory (via pwd's stdout).
func runBrokerPwd(t *testing.T, tc sandbox.TokenContext, shimCwd string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	broker := &sandbox.Broker{SocketPath: sockPath}

	token := broker.Register(map[string]sandbox.CommandDef{
		"/bin/pwd": {Name: "pwd", Path: "/bin/pwd", AllowedPatterns: []string{"*"}},
	}, nil, tc)
	defer broker.Unregister(token)

	if err := broker.Start(ctx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	defer broker.Stop()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := sandbox.ExecRequest{Command: "/bin/pwd", Cwd: shimCwd, Token: token}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp sandbox.ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit=%d, stderr=%s", resp.ExitCode, resp.Stderr)
	}
	return strings.TrimSpace(resp.Stdout)
}

// TestBroker_HostCommandRunsInWorktreeRoot ensures that when the token context
// advertises a worktree, the broker runs host commands inside it regardless of
// what cwd the sandbox-side shim reports. gate jobs depend on this: their
// sandbox cwd is a tmpfs HOME that on the host side is the user's real HOME
// (no repo metadata), so tools like `gh pr create` would otherwise fail with
// "not a git repository".
func TestBroker_HostCommandRunsInWorktreeRoot(t *testing.T) {
	worktree := t.TempDir()
	tc := sandbox.TokenContext{
		JobID:       "job-host-cwd",
		TaskID:      "task-host-cwd",
		ProjectID:   "proj-1",
		Role:        testRoleGate,
		ProjectDir:  t.TempDir(), // must be ignored when WorktreeDir is set
		WorktreeDir: worktree,
	}
	got := runBrokerPwd(t, tc, "/home/someone")
	if got != worktree {
		t.Errorf("host command cwd = %q, want worktree root %q", got, worktree)
	}
}

// TestBroker_HostCommandFallsBackToProjectDir covers the no-worktree case: a
// job without a task worktree still gets its host commands rooted in the
// project work dir (the broker's default host-side context).
func TestBroker_HostCommandFallsBackToProjectDir(t *testing.T) {
	projectDir := t.TempDir()
	tc := sandbox.TokenContext{
		JobID:      "job-proj-cwd",
		TaskID:     "task-proj-cwd",
		ProjectID:  "proj-1",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	got := runBrokerPwd(t, tc, "/home/someone")
	if got != projectDir {
		t.Errorf("host command cwd = %q, want project dir %q", got, projectDir)
	}
}

// TestBroker_HostCommandRespectsRequestCwdWhenContextEmpty ensures the shim's
// reported cwd is honoured when neither worktree nor project is known — this
// is the `boid exec` path where the sandbox maps into the real project tree
// and sandbox cwd is already meaningful on the host.
func TestBroker_HostCommandRespectsRequestCwdWhenContextEmpty(t *testing.T) {
	shimCwd := t.TempDir()
	tc := sandbox.TokenContext{
		JobID:     "job-shim-cwd",
		TaskID:    "task-shim-cwd",
		ProjectID: "proj-1",
		Role:      testRoleHook,
	}
	got := runBrokerPwd(t, tc, shimCwd)
	if got != shimCwd {
		t.Errorf("host command cwd = %q, want shim cwd %q", got, shimCwd)
	}
}

func TestBroker_UnknownCommand(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"/bin/echo": {Name: "echo", Path: "/bin/echo"},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/bin/rm",
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
		"/bin/echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/bin/echo",
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
		"/bin/echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"hello"},
	})
	if resp.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", resp.ExitCode)
	}
}

func TestBroker_Unregister(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"/bin/echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	}, nil, testCtx)

	// Before unregister: should work
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Errorf("before unregister: exit code = %d, want 0; stderr: %s", resp.ExitCode, resp.Stderr)
	}

	broker.Unregister(token)

	// After unregister: should fail
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if resp.ExitCode != 1 {
		t.Errorf("after unregister: exit code = %d, want 1", resp.ExitCode)
	}
}

// TestBroker_HostCommandWithAliasedPath は host_commands.<name>.path で別名
// (例: run-e2e: path: e2e/run.sh) を使うケースの回帰テスト。dispatcher の
// ResolveHostCommands が絶対パスを Commands map のキーにし、shim も同じ絶対
// パスを Command に詰めるので、key と request の絶対パスが一致して lookup
// が成功する。argv0 の basename (run.sh) は使われない。
func TestBroker_HostCommandWithAliasedPath(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "e2e", "run.sh")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho aliased\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		scriptPath: {
			Name:            "run-e2e",
			Path:            scriptPath,
			AllowedPatterns: []string{"*"},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: scriptPath,
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "aliased") {
		t.Errorf("stdout = %q, want contain 'aliased'", resp.Stdout)
	}
}

// TestBroker_EmptyPathFallsBackToName covers the broker's defensive fallback
// when CommandDef.Path is left blank. In production dispatcher.ResolveHostCommands
// always fills Path with an absolute path, so this only exercises the broker's
// internal robustness: Commands map is keyed by the shim mount target (absolute
// path), but the actual binary is resolved via $PATH using Name.
func TestBroker_EmptyPathFallsBackToName(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"/bin/echo": {
			Name:            "echo",
			AllowedPatterns: []string{"*"},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"ok"},
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "ok\n" {
		t.Errorf("stdout = %q, want %q", resp.Stdout, "ok\n")
	}
}

// TestBroker_EmptyPathUnresolvableNameReportsClearError covers the case
// where Path is blank and Name is not on $PATH. The broker must surface a
// clear error instead of an opaque fork/exec failure.
func TestBroker_EmptyPathUnresolvableNameReportsClearError(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"/usr/local/bin/boid-nonexistent-binary-xyzzy": {
			Name:            "boid-nonexistent-binary-xyzzy",
			AllowedPatterns: []string{"*"},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/local/bin/boid-nonexistent-binary-xyzzy",
		Token:   token,
	})
	if resp.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "boid-nonexistent-binary-xyzzy") {
		t.Errorf("expected stderr to mention the command name, got: %q", resp.Stderr)
	}
}

func TestBroker_PerCommandEnv(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"/usr/bin/env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"TEST_VAR": "hello123"},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/bin/env",
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
	// Commands map is keyed by the absolute shim mount target (= host git
	// binary path). When the entry has no git builtin policy attached, the
	// broker falls through to host_commands lookup.
	token := broker.Register(map[string]sandbox.CommandDef{
		"/usr/bin/git": {
			Name:               "git",
			Path:               "/bin/echo",
			AllowedSubcommands: []string{"push"},
		},
	}, nil, sandbox.TokenContext{
		JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1", Role: testRoleGate,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/bin/git",
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
		"/usr/bin/env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"GH_TOKEN": "secret:github/pat", "PLAIN": "value"},
		},
	}, nil, sandbox.TokenContext{
		JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1", Role: testRoleGate,
	}, resolver)
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/bin/env",
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
		"/usr/bin/env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"GH_TOKEN": "secret:", "PLAIN": "value"},
		},
	}, nil, sandbox.TokenContext{
		JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1", Role: testRoleGate,
	}, resolver)
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/bin/env",
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

// TestBroker_SecretMissing_RejectsExecution verifies fail-closed behavior:
// when a host_command declares an env var via "secret:" but the secret can't
// be resolved, the command must be rejected at exec time rather than silently
// dropping the env entry (which would let host-side fallbacks like
// ~/.config/gh/hosts.yml or inherited host env take over an intended-isolated
// invocation).
func TestBroker_SecretMissing_RejectsExecution(t *testing.T) {
	broker := &sandbox.Broker{}
	resolver := func(key string) (string, error) {
		return "", fmt.Errorf("not found: %s", key)
	}

	token := broker.RegisterWithSecrets(map[string]sandbox.CommandDef{
		"/usr/bin/env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"GH_TOKEN": "secret:github/pat", "PLAIN": "value"},
		},
	}, nil, sandbox.TokenContext{
		JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1", Role: testRoleGate,
	}, resolver)
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/bin/env",
		Token:   token,
	})
	if resp.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code (fail-closed); stdout=%q stderr=%q", resp.Stdout, resp.Stderr)
	}
	if !strings.Contains(resp.Stderr, "GH_TOKEN") {
		t.Errorf("expected GH_TOKEN env name in stderr, got: %s", resp.Stderr)
	}
	if !strings.Contains(resp.Stderr, "github/pat") {
		t.Errorf("expected secret key github/pat in stderr, got: %s", resp.Stderr)
	}
	if strings.Contains(resp.Stdout, "GH_TOKEN=") || strings.Contains(resp.Stdout, "PLAIN=value") {
		t.Errorf("command should not have executed; stdout=%q", resp.Stdout)
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
		Role:              testRoleGate,
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
	if got.Role != testRoleGate {
		t.Errorf("Role = %q, want %q", got.Role, testRoleGate)
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
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), hookCtx)

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
			Op: sandbox.BoidOpTaskCreate,
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("hook task create should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// task_ask leaves TaskID empty in the shim; the broker fills it from the token
// context (the agent's own task) before dispatching to the executor, and passes
// the question through. The executor (not the broker) blocks for the answer.
func TestBroker_TaskAsk_FillsTaskIDFromContext(t *testing.T) {
	exec := &fakeBoidExecutor{resp: &sandbox.ExecResponse{Stdout: "go ahead"}}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	hookCtx := sandbox.TokenContext{
		JobID:      "j1",
		TaskID:     "t1",
		ProjectID:  "p1",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	policies := map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpTaskAsk): {},
		}},
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, policies, hookCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:       sandbox.BoidOpTaskAsk,
			Question: "Proceed?",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "go ahead" {
		t.Fatalf("stdout = %q, want the executor's answer", resp.Stdout)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].Op != sandbox.BoidOpTaskAsk {
		t.Fatalf("op = %q, want task_ask", exec.calls[0].Op)
	}
	if exec.calls[0].TaskID != "t1" {
		t.Fatalf("task id = %q, want t1 (filled from token context)", exec.calls[0].TaskID)
	}
	if exec.calls[0].Question != "Proceed?" {
		t.Fatalf("question = %q, want Proceed?", exec.calls[0].Question)
	}
}

// A task_ask with no question (defensive: the shim already rejects this) is
// refused by the broker before reaching the executor.
func TestBroker_TaskAsk_RejectsEmptyQuestion(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	hookCtx := sandbox.TokenContext{
		JobID:      "j1",
		TaskID:     "t1",
		ProjectID:  "p1",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	policies := map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{string(sandbox.BoidOpTaskAsk): {}}},
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, policies, hookCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAsk},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "requires a question") {
		t.Fatalf("empty question should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called for an invalid ask, got %d calls", len(exec.calls))
	}
}

// ctxBlockingExecutor mimics the blocking AskTaskBlocking handler: it blocks
// until its context is cancelled, signalling start and release so a test can
// observe the broker's per-connection cancellation.
type ctxBlockingExecutor struct {
	started  chan struct{}
	released chan struct{}
}

func (e *ctxBlockingExecutor) ExecuteBoidBuiltin(ctx context.Context, _ sandbox.TokenContext, _ *sandbox.BoidRequest) *sandbox.ExecResponse {
	close(e.started)
	<-ctx.Done()
	close(e.released)
	return &sandbox.ExecResponse{ExitCode: 1, Stderr: "canceled"}
}

// When the sandbox closes its connection mid-ask (process death), the broker's
// connection-close watcher must cancel the request context so the blocking
// handler unblocks instead of leaking. Exercises the live handleConn path.
func TestBroker_TaskAsk_ConnCloseCancelsContext(t *testing.T) {
	exec := &ctxBlockingExecutor{started: make(chan struct{}), released: make(chan struct{})}
	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	broker := &sandbox.Broker{SocketPath: sockPath, BoidExecutor: exec}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := broker.Start(ctx); err != nil {
		t.Fatalf("start broker: %v", err)
	}

	projectDir := t.TempDir()
	policies := map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{string(sandbox.BoidOpTaskAsk): {}}},
	}
	token := broker.Register(nil, policies, sandbox.TokenContext{
		JobID:      "j1",
		TaskID:     "t1",
		ProjectID:  "p1",
		ProjectDir: projectDir,
	})

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	req := sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAsk, Question: "Q?"},
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("executor never started blocking")
	}

	// Simulate sandbox death: close the connection without reading the response.
	conn.Close()

	select {
	case <-exec.released:
		// good: the per-connection context was cancelled by the close watcher.
	case <-time.After(2 * time.Second):
		t.Fatal("blocking executor was not cancelled when the connection closed")
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
		Role:       testRoleGate,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

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
			Op: sandbox.BoidOpTaskCreate,
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
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:        sandbox.BoidOpTaskCreate,
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
// task の artifact.auto-merge.pr を書き戻すユースケース)。
func TestBroker_BoidBuiltinPolicy_GateRoleTaskUpdate(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:      "j-gate",
		TaskID:     "t-gate",
		ProjectID:  "p1",
		Role:       testRoleGate,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

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
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), hookCtx)

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
		Role:       testRoleGate,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

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

// BoidOpAgentStop は own job 以外への配送を拒否する。 job_done と同じガード。
// agent stop は 「自分の agent (claude) を止めて」 という意図なので、 他の
// runtime に SIGUSR1 を撃ち込めると agent クロストークの hijack 経路になる。
func TestBroker_BoidBuiltinAgentStopRejectsWrongJob(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:      "job-keep",
		TaskID:     "task-keep",
		ProjectID:  "proj-keep",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:    sandbox.BoidOpAgentStop,
			JobID: "other-job",
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current job") {
		t.Fatalf("expected job id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not receive cross-job agent stop, calls=%d", len(exec.calls))
	}

	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:    sandbox.BoidOpAgentStop,
			JobID: "",
		},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "requires a job id") {
		t.Fatalf("expected missing job id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}

	// 正規ケース: own job への agent stop は executor に届く。
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:    sandbox.BoidOpAgentStop,
			JobID: "job-keep",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("self-job agent stop should succeed, exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should receive own-job agent stop, calls=%d", len(exec.calls))
	}
	if exec.calls[0].Op != sandbox.BoidOpAgentStop || exec.calls[0].JobID != "job-keep" {
		t.Fatalf("unexpected executor call: %+v", exec.calls[0])
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
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), ctx)

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

// req.Boid を伴わない boid binary 名義のリクエストは boid builtin 経路に
// 入らず、絶対パスキーの host command lookup でも見つからないので
// "command not allowed" として reject される（旧仕様の typed-request エラー
// 経路を新設計の dispatch ルールに置き換えたもの）。
func TestBroker_BoidBuiltinWithoutTypedPayloadIsRejected(t *testing.T) {
	broker := &sandbox.Broker{}
	cwd := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), sandbox.TokenContext{
		JobID:      "job-1",
		TaskID:     "task-1",
		ProjectID:  "proj-1",
		Role:       testRoleHook,
		ProjectDir: cwd,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/local/bin/boid",
		Cwd:     cwd,
		Token:   token,
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("expected reject, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
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
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), hookCtx)

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

// gate role は BoidOpTaskReopen を実行できる（github-auto-merge kit がコンフリクト検出時に done → reworking に戻すユースケース）。
func TestBroker_BoidBuiltinPolicy_GateRoleAllowsReopen(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:      "j-gate",
		TaskID:     "t-gate",
		ProjectID:  "p1",
		Role:       testRoleGate,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

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
		Role:       testRoleGate,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

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
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), hookCtx)

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
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed by policy") {
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
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

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
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

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

func TestBroker_BoidTaskCreate_ResolvesProjectRef(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{
		BoidExecutor: exec,
		ProjectResolver: func(ref string) (string, error) {
			if ref == "mera-ui" {
				return "p2", nil
			}
			return ref, nil
		},
	}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "p1",
		WorkspaceID:       "ws-1",
		AllowedProjectIDs: []string{"p1", "p2"},
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:        sandbox.BoidOpTaskCreate,
			ProjectID: "mera-ui", // 名前指定
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("name-based create should be accepted after resolution, exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].ProjectID != "p2" {
		t.Fatalf("executor received project_id = %q, want resolved UUID %q", exec.calls[0].ProjectID, "p2")
	}
}

// TestBroker_BoidTaskCreate_CreatePatchNameNotMutated はシム由来のリクエスト構造を再現する。
// シムは BoidRequest.ProjectID と CreatePatch.project_id の両方に元の名前をセットするが、
// broker は BoidRequest.ProjectID のみを UUID に解決し CreatePatch は書き換えない。
// executor には ProjectID=UUID が届き、CreatePatch.project_id=名前 のまま渡される。
func TestBroker_BoidTaskCreate_CreatePatchNameNotMutated(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{
		BoidExecutor: exec,
		ProjectResolver: func(ref string) (string, error) {
			if ref == "boid-kits" {
				return "dad1961a-9ef9-495d-858f-e27e75d9afca", nil
			}
			return ref, nil
		},
	}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "boid-main-uuid",
		WorkspaceID:       "ws-boid",
		AllowedProjectIDs: []string{"boid-main-uuid", "dad1961a-9ef9-495d-858f-e27e75d9afca"},
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	// シムが生成するリクエスト: ProjectID とCreatePatch.project_id が同じ名前
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:          sandbox.BoidOpTaskCreate,
			ProjectID:   "boid-kits",
			CreatePatch: json.RawMessage(`{"project_id":"boid-kits","title":"peer task","behavior":"dev"}`),
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("name-based create with CreatePatch should succeed, exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	// broker は ProjectID を UUID に解決して executor に渡す
	if exec.calls[0].ProjectID != "dad1961a-9ef9-495d-858f-e27e75d9afca" {
		t.Fatalf("executor ProjectID = %q, want resolved UUID", exec.calls[0].ProjectID)
	}
	// CreatePatch は broker によって書き換えられない (executor 側で req.ProjectID を優先すべき)
	var patch struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(exec.calls[0].CreatePatch, &patch); err != nil {
		t.Fatalf("unmarshal CreatePatch: %v", err)
	}
	if patch.ProjectID != "boid-kits" {
		t.Fatalf("CreatePatch.project_id = %q, want original name %q (broker does not rewrite CreatePatch)", patch.ProjectID, "boid-kits")
	}
}

func TestBroker_BoidTaskCreate_ResolverErrorSurfaced(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{
		BoidExecutor: exec,
		ProjectResolver: func(ref string) (string, error) {
			return "", fmt.Errorf("no project matches ref %q", ref)
		},
	}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "p1",
		AllowedProjectIDs: []string{"p1"},
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:        sandbox.BoidOpTaskCreate,
			ProjectID: "bogus-name",
		},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("resolver error should fail task create, got exit=%d", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "bogus-name") {
		t.Fatalf("resolver error should surface ref name, got stderr=%q", resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called on resolver error, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskCreate_PassthroughWhenResolverNil(t *testing.T) {
	// ProjectResolver が nil のときは UUID をそのまま使う (既存挙動互換)
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "p1",
		AllowedProjectIDs: []string{"p1", "p2"},
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:        sandbox.BoidOpTaskCreate,
			ProjectID: "p2",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("UUID passthrough should succeed, exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].ProjectID != "p2" {
		t.Fatalf("executor call = %+v, want ProjectID=p2", exec.calls)
	}
}

func TestBroker_BoidTaskImport_ResolvesProjectRefs(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{
		BoidExecutor: exec,
		ProjectResolver: func(ref string) (string, error) {
			switch ref {
			case "mera-ui":
				return "p2", nil
			case "rook-server":
				return "p1", nil
			}
			return ref, nil
		},
	}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "p1",
		AllowedProjectIDs: []string{"p1", "p2"},
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskImport,
			ImportTasks: []json.RawMessage{
				json.RawMessage(`{"project_id":"mera-ui","title":"fe"}`),
				json.RawMessage(`{"project_id":"rook-server","title":"be"}`),
			},
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("import with name refs should succeed, exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if len(exec.calls[0].ImportTasks) != 2 {
		t.Fatalf("import tasks = %d, want 2", len(exec.calls[0].ImportTasks))
	}
	// project_id が解決後 UUID に書き換わって executor に届くことを確認
	var first, second struct {
		ProjectID string `json:"project_id"`
		Title     string `json:"title"`
	}
	if err := json.Unmarshal(exec.calls[0].ImportTasks[0], &first); err != nil {
		t.Fatalf("unmarshal 0: %v", err)
	}
	if err := json.Unmarshal(exec.calls[0].ImportTasks[1], &second); err != nil {
		t.Fatalf("unmarshal 1: %v", err)
	}
	if first.ProjectID != "p2" || first.Title != "fe" {
		t.Fatalf("task[0] = %+v, want project_id=p2 title=fe", first)
	}
	if second.ProjectID != "p1" || second.Title != "be" {
		t.Fatalf("task[1] = %+v, want project_id=p1 title=be", second)
	}
}

func TestBroker_BoidTaskImport_ResolvesImportProjectOverride(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{
		BoidExecutor: exec,
		ProjectResolver: func(ref string) (string, error) {
			if ref == "mera-ui" {
				return "p2", nil
			}
			return ref, nil
		},
	}
	projectDir := t.TempDir()
	gateCtx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "p1",
		AllowedProjectIDs: []string{"p1", "p2"},
		Role:              testRoleGate,
		ProjectDir:        projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskImport,
			ImportTasks: []json.RawMessage{
				json.RawMessage(`{"title":"t"}`),
			},
			ImportProjectOverride: "mera-ui",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("override name ref should succeed, exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].ImportProjectOverride != "p2" {
		t.Fatalf("executor ImportProjectOverride = %q, want %q", exec.calls[0].ImportProjectOverride, "p2")
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
		Role:       testRoleGate,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), gateCtx)

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

// --- BoidOpTaskList tests ---

func newBrokerForListTest(t *testing.T) (*sandbox.Broker, *fakeBoidExecutor) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{
		SocketPath:   sockPath,
		BoidExecutor: exec,
		ProjectResolver: func(ref string) (string, error) {
			return ref, nil
		},
	}
	boidCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := broker.Start(boidCtx); err != nil {
		t.Fatalf("broker start: %v", err)
	}
	return broker, exec
}

func TestBroker_BoidTaskList_ProjectIDAllowed(t *testing.T) {
	// project_id 指定 + 同 workspace (AllowedProjectIDs に含まれる) → OK
	ctx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "proj-1",
		WorkspaceID:       "ws-1",
		AllowedProjectIDs: []string{"proj-1", "proj-2"},
		Role:              testRoleGate,
	}
	broker, exec := newBrokerForListTest(t)
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:        sandbox.BoidOpTaskList,
			ProjectID: "proj-2",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("same-workspace project should be allowed, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].ProjectID != "proj-2" {
		t.Fatalf("executor should be called with ProjectID=proj-2, calls=%+v", exec.calls)
	}
}

func TestBroker_BoidTaskList_ProjectIDDenied(t *testing.T) {
	// project_id 指定 + 異 workspace → エラー
	ctx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "proj-1",
		WorkspaceID:       "ws-1",
		AllowedProjectIDs: []string{"proj-1"},
		Role:              testRoleGate,
	}
	broker, exec := newBrokerForListTest(t)
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:        sandbox.BoidOpTaskList,
			ProjectID: "other-proj",
		},
	})
	if resp.ExitCode == 0 {
		t.Fatal("cross-workspace project_id should be denied")
	}
	if !strings.Contains(resp.Stderr, "workspace") {
		t.Errorf("error should mention workspace, got %q", resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatal("executor should not be called on denied request")
	}
}

func TestBroker_BoidTaskList_WorkspaceIDMatch(t *testing.T) {
	// workspace_id 指定 + 同 workspace → OK
	ctx := sandbox.TokenContext{
		JobID:       "j1",
		TaskID:      "t1",
		ProjectID:   "proj-1",
		WorkspaceID: "ws-1",
		Role:        testRoleGate,
	}
	broker, exec := newBrokerForListTest(t)
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:          sandbox.BoidOpTaskList,
			WorkspaceID: "ws-1",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("matching workspace_id should succeed, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].WorkspaceID != "ws-1" {
		t.Fatalf("executor should be called with WorkspaceID=ws-1, calls=%+v", exec.calls)
	}
}

func TestBroker_BoidTaskList_WorkspaceIDMismatch(t *testing.T) {
	// workspace_id 指定 + 異 workspace → エラー
	ctx := sandbox.TokenContext{
		JobID:       "j1",
		TaskID:      "t1",
		ProjectID:   "proj-1",
		WorkspaceID: "ws-1",
		Role:        testRoleGate,
	}
	broker, exec := newBrokerForListTest(t)
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:          sandbox.BoidOpTaskList,
			WorkspaceID: "ws-other",
		},
	})
	if resp.ExitCode == 0 {
		t.Fatal("mismatched workspace_id should be denied")
	}
	if !strings.Contains(resp.Stderr, "workspace") {
		t.Errorf("error should mention workspace, got %q", resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatal("executor should not be called on denied request")
	}
}

func TestBroker_BoidTaskList_AutoInjectWorkspaceID(t *testing.T) {
	// 両方未指定 + entry.WorkspaceID 非空 → WorkspaceID を自動 inject
	ctx := sandbox.TokenContext{
		JobID:       "j1",
		TaskID:      "t1",
		ProjectID:   "proj-1",
		WorkspaceID: "ws-auto",
		Role:        testRoleGate,
	}
	broker, exec := newBrokerForListTest(t)
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskList,
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("auto inject should succeed, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].WorkspaceID != "ws-auto" {
		t.Fatalf("executor should be called with WorkspaceID=ws-auto injected, calls=%+v", exec.calls)
	}
}

func TestBroker_BoidTaskList_AutoInjectAllowedProjectIDs(t *testing.T) {
	// 両方未指定 + entry.WorkspaceID 空 → AllowedProjectIDs フィルタ (executor 側、broker は inject しない)
	ctx := sandbox.TokenContext{
		JobID:             "j1",
		TaskID:            "t1",
		ProjectID:         "proj-1",
		WorkspaceID:       "",
		AllowedProjectIDs: []string{"proj-1"},
		Role:              testRoleGate,
	}
	broker, exec := newBrokerForListTest(t)
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op: sandbox.BoidOpTaskList,
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("empty workspace should succeed with AllowedProjectIDs filter, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should be called once, calls=%d", len(exec.calls))
	}
	// WorkspaceID は inject されない (空のまま executor が AllowedProjectIDs を使う)
	if exec.calls[0].WorkspaceID != "" {
		t.Errorf("WorkspaceID should remain empty for AllowedProjectIDs path, got %q", exec.calls[0].WorkspaceID)
	}
	if exec.calls[0].ProjectID != "" {
		t.Errorf("ProjectID should remain empty for AllowedProjectIDs path, got %q", exec.calls[0].ProjectID)
	}
}

func testAnswerBoidPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {
			AllowedOps: map[string]struct{}{
				string(sandbox.BoidOpTaskAnswer): {},
			},
			AllowedCwdRoots: []string{"/tmp"},
		},
	}
}

func TestBroker_BoidTaskAnswer_Dispatched(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	ctx := sandbox.TokenContext{
		JobID:     "j1",
		TaskID:    "t1",
		ProjectID: "p1",
		Role:      testRoleHook,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testAnswerBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid: &sandbox.BoidRequest{
			Op:         sandbox.BoidOpTaskAnswer,
			TaskID:     "task-abc",
			QuestionID: "q-1",
			Answer:     "yes",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	got := exec.calls[0]
	if got.Op != sandbox.BoidOpTaskAnswer || got.TaskID != "task-abc" || got.QuestionID != "q-1" || got.Answer != "yes" {
		t.Errorf("unexpected request: %+v", got)
	}
}

func TestBroker_BoidTaskAnswer_RequiresTaskID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	token := broker.Register(map[string]sandbox.CommandDef{}, testAnswerBoidPolicies(), sandbox.TokenContext{Role: testRoleHook})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAnswer, QuestionID: "q-1", Answer: "yes"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "task id") {
		t.Fatalf("expected task id error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskAnswer_RequiresQuestionID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	token := broker.Register(map[string]sandbox.CommandDef{}, testAnswerBoidPolicies(), sandbox.TokenContext{Role: testRoleHook})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAnswer, TaskID: "t1", Answer: "yes"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "question id") {
		t.Fatalf("expected question id error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskAnswer_RequiresAnswer(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	token := broker.Register(map[string]sandbox.CommandDef{}, testAnswerBoidPolicies(), sandbox.TokenContext{Role: testRoleHook})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAnswer, TaskID: "t1", QuestionID: "q-1"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "answer") {
		t.Fatalf("expected answer error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskAnswer_PolicyReject(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	// testHookBoidPolicies には BoidOpTaskAnswer が含まれない
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleHook,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAnswer, TaskID: "t1", QuestionID: "q-1", Answer: "yes"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("expected policy rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called, calls=%d", len(exec.calls))
	}
}

// --- action_send broker tests ---

func TestBroker_BoidActionSend_RequiresTaskID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleGate,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpActionSend, ActionType: "reopen"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "task id") {
		t.Fatalf("expected task id error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}

func TestBroker_BoidActionSend_RequiresType(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleGate,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpActionSend, TaskID: "t1"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "type") {
		t.Fatalf("expected type error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}

func TestBroker_BoidActionSend_PolicyReject(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	// testHookBoidPolicies には BoidOpActionSend が含まれない
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleHook,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpActionSend, TaskID: "t1", ActionType: "reopen"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("expected policy rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}

// --- job_list broker tests ---

func TestBroker_BoidJobList_RequiresTaskID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleGate,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpJobList},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "task id") {
		t.Fatalf("expected task id error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}

func TestBroker_BoidJobList_PolicyReject(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	// testHookBoidPolicies には BoidOpJobList が含まれない
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleHook,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpJobList, TaskID: "t1"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("expected policy rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}

// --- job_show broker tests ---

func TestBroker_BoidJobShow_RequiresJobID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleGate,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpJobShow},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "job id") {
		t.Fatalf("expected job id error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}

func TestBroker_BoidJobShow_PolicyReject(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	// testHookBoidPolicies には BoidOpJobShow が含まれない
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleHook,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpJobShow, JobID: "job-1"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("expected policy rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}

// --- job_log broker tests ---

func TestBroker_BoidJobLog_RequiresJobID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleGate,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     "/tmp",
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpJobLog},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "job id") {
		t.Fatalf("expected job id error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}

func TestBroker_BoidJobLog_PolicyReject(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	// testHookBoidPolicies には BoidOpJobLog が含まれない
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), sandbox.TokenContext{
		Role:       testRoleHook,
		ProjectDir: projectDir,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpJobLog, JobID: "job-1"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("expected policy rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be called")
	}
}
