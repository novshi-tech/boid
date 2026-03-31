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
	JobID:     "job-1",
	TaskID:    "task-1",
	ProjectID: "proj-1",
	Role:      string(projectspec.RoleHook),
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
	}, testCtx)
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
	}, testCtx)

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
	}, testCtx)

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
	}, testCtx)

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
	}, testCtx)

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

func TestBroker_CwdValidation(t *testing.T) {
	tmpDir := t.TempDir()

	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"echo": {
			Name:               "echo",
			Path:               "/bin/echo",
			AllowedPatterns:    []string{"*"},
			RequireCwd:         true,
			AllowedCwdPrefixes: []string{tmpDir},
		},
	}, testCtx)

	// Valid cwd
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
		Cwd:     tmpDir,
	})
	if resp.ExitCode != 0 {
		t.Errorf("valid cwd: exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	// Missing cwd when required
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if resp.ExitCode != 1 {
		t.Error("expected rejection for missing cwd")
	}

	// Cwd outside allowed prefixes
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
		Cwd:     "/tmp/evil",
	})
	if resp.ExitCode != 1 {
		t.Error("expected rejection for cwd outside allowed prefixes")
	}

	// Relative path should be rejected
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
		Cwd:     "relative/path",
	})
	if resp.ExitCode != 1 {
		t.Error("expected rejection for relative cwd")
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
	}, testCtx)

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
	}, sandbox.TokenContext{
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

func TestBroker_RegisterReturnsUniqueTokens(t *testing.T) {
	broker := &sandbox.Broker{}
	cmds := map[string]sandbox.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo"},
	}

	t1 := broker.Register(cmds, testCtx)
	t2 := broker.Register(cmds, testCtx)
	if t1 == t2 {
		t.Error("Register should return unique tokens")
	}
}

func TestBroker_GetContext(t *testing.T) {
	broker := &sandbox.Broker{}
	ctx := sandbox.TokenContext{
		JobID:     "job-42",
		TaskID:    "task-99",
		ProjectID: "proj-7",
		Role:      string(projectspec.RoleGate),
	}

	token := broker.Register(map[string]sandbox.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo"},
	}, ctx)

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

	// Invalid token
	_, ok = broker.GetContext("nonexistent")
	if ok {
		t.Error("expected GetContext to return false for invalid token")
	}
}

func TestBroker_BoidBuiltinPolicy_HookRole(t *testing.T) {
	broker := &sandbox.Broker{}
	hookCtx := sandbox.TokenContext{
		JobID: "j1", TaskID: "t1", ProjectID: "p1", Role: string(projectspec.RoleHook),
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, hookCtx)

	// hook can call: boid job done
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Args:    []string{"job", "done", "--exit-code", "0"},
		Token:   token,
	})
	// The actual command execution may fail (no boid binary at expected path),
	// but it should NOT be rejected by policy. Check that it's not "command not allowed".
	if resp.Stderr == "command not allowed: boid" {
		t.Error("hook should be allowed to call boid job done")
	}

	// hook cannot call: boid task create
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Args:    []string{"task", "create", "--title", "test"},
		Token:   token,
	})
	if !strings.Contains(resp.Stderr, "not allowed") {
		t.Errorf("hook should NOT be allowed to call boid task create, stderr: %q", resp.Stderr)
	}
}

func TestBroker_BoidBuiltinPolicy_GateRole(t *testing.T) {
	broker := &sandbox.Broker{}
	gateCtx := sandbox.TokenContext{
		JobID: "j1", TaskID: "t1", ProjectID: "p1", Role: string(projectspec.RoleGate),
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, gateCtx)

	// gate can call: boid job done
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Args:    []string{"job", "done", "--exit-code", "0"},
		Token:   token,
	})
	if resp.Stderr == "command not allowed: boid" {
		t.Error("gate should be allowed to call boid job done")
	}

	// gate can call: boid task create
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Args:    []string{"task", "create", "--title", "test"},
		Token:   token,
	})
	if strings.Contains(resp.Stderr, "not allowed") {
		t.Error("gate should be allowed to call boid task create")
	}
}
