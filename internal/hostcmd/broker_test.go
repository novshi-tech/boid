package hostcmd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/hostcmd"
)

func TestBroker_ExecCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	broker := &hostcmd.Broker{SocketPath: sockPath}

	token := broker.Register(map[string]hostcmd.CommandDef{
		"echo": {
			Name:            "echo",
			Path:            "/bin/echo",
			AllowedPatterns: []string{"*"},
		},
	})
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

	req := hostcmd.ExecRequest{
		Command: "echo",
		Args:    []string{"hello", "world"},
		Token:   token,
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp hostcmd.ExecResponse
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
	broker := &hostcmd.Broker{}
	token := broker.Register(map[string]hostcmd.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo"},
	})

	resp := broker.Handle(&hostcmd.ExecRequest{
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
	broker := &hostcmd.Broker{}
	broker.Register(map[string]hostcmd.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	})

	resp := broker.Handle(&hostcmd.ExecRequest{
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
	broker := &hostcmd.Broker{}
	broker.Register(map[string]hostcmd.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	})

	resp := broker.Handle(&hostcmd.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
	})
	if resp.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", resp.ExitCode)
	}
}

func TestBroker_Unregister(t *testing.T) {
	broker := &hostcmd.Broker{}
	token := broker.Register(map[string]hostcmd.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
	})

	// Before unregister: should work
	resp := broker.Handle(&hostcmd.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Errorf("before unregister: exit code = %d, want 0; stderr: %s", resp.ExitCode, resp.Stderr)
	}

	broker.Unregister(token)

	// After unregister: should fail
	resp = broker.Handle(&hostcmd.ExecRequest{
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

	broker := &hostcmd.Broker{}
	token := broker.Register(map[string]hostcmd.CommandDef{
		"echo": {
			Name:               "echo",
			Path:               "/bin/echo",
			AllowedPatterns:    []string{"*"},
			RequireCwd:         true,
			AllowedCwdPrefixes: []string{tmpDir},
		},
	})

	// Valid cwd
	resp := broker.Handle(&hostcmd.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
		Cwd:     tmpDir,
	})
	if resp.ExitCode != 0 {
		t.Errorf("valid cwd: exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	// Missing cwd when required
	resp = broker.Handle(&hostcmd.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if resp.ExitCode != 1 {
		t.Error("expected rejection for missing cwd")
	}

	// Cwd outside allowed prefixes
	resp = broker.Handle(&hostcmd.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
		Cwd:     "/tmp/evil",
	})
	if resp.ExitCode != 1 {
		t.Error("expected rejection for cwd outside allowed prefixes")
	}

	// Relative path should be rejected
	resp = broker.Handle(&hostcmd.ExecRequest{
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
	broker := &hostcmd.Broker{}
	token := broker.Register(map[string]hostcmd.CommandDef{
		"env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"TEST_VAR": "hello123"},
		},
	})

	resp := broker.Handle(&hostcmd.ExecRequest{
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
	broker := &hostcmd.Broker{}
	resolver := func(key string) (string, error) {
		if key == "github/pat" {
			return "ghp_secret123", nil
		}
		return "", fmt.Errorf("not found: %s", key)
	}

	token := broker.RegisterWithSecrets(map[string]hostcmd.CommandDef{
		"env": {
			Name:            "env",
			Path:            "/usr/bin/env",
			AllowedPatterns: []string{"*"},
			Env:             map[string]string{"GH_TOKEN": "secret:github/pat", "PLAIN": "value"},
		},
	}, resolver)

	resp := broker.Handle(&hostcmd.ExecRequest{
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
	broker := &hostcmd.Broker{}
	cmds := map[string]hostcmd.CommandDef{
		"echo": {Name: "echo", Path: "/bin/echo"},
	}

	t1 := broker.Register(cmds)
	t2 := broker.Register(cmds)
	if t1 == t2 {
		t.Error("Register should return unique tokens")
	}
}
