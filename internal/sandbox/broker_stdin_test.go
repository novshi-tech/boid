package sandbox_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestBroker_RejectsStdinWhenNotAllowed(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"cat": {
			Name:            "cat",
			Path:            "/bin/cat",
			AllowedPatterns: []string{"*"},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "cat",
		Token:   token,
		Stdin:   []byte("secret input"),
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "stdin not allowed") {
		t.Fatalf("stderr = %q, want stdin rejection", resp.Stderr)
	}
}

func TestBroker_AllowsStdinWhenConfigured(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"cat": {
			Name:            "cat",
			Path:            "/bin/cat",
			AllowedPatterns: []string{"*"},
			AllowStdin:      true,
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "cat",
		Token:   token,
		Stdin:   []byte("hello stdin"),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "hello stdin" {
		t.Fatalf("stdout = %q, want %q", resp.Stdout, "hello stdin")
	}
}

