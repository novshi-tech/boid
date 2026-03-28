package hostcmd_test

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/hostcmd"
)

func TestBroker_ExecCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	broker := &hostcmd.Broker{
		SocketPath: sockPath,
		Commands: map[string]hostcmd.CommandDef{
			"echo": {
				Name:            "echo",
				Path:            "/bin/echo",
				AllowedPatterns: []string{"*"},
			},
		},
	}

	if err := broker.Start(ctx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	defer broker.Stop()

	// Send request via unix socket
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	defer conn.Close()

	req := hostcmd.ExecRequest{
		Command: "echo",
		Args:    []string{"hello", "world"},
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
	broker := &hostcmd.Broker{
		Commands: map[string]hostcmd.CommandDef{
			"echo": {
				Name: "echo",
				Path: "/bin/echo",
			},
		},
	}

	req := &hostcmd.ExecRequest{
		Command: "rm",
		Args:    []string{"-rf", "/"},
	}

	resp := broker.Handle(req)
	if resp.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", resp.ExitCode)
	}
	if resp.Stderr == "" {
		t.Error("expected non-empty stderr for unknown command")
	}
}
