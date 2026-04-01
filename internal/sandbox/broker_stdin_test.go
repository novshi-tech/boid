package sandbox_test

import (
	"os"
	"path/filepath"
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

func TestBroker_CwdMustExistAndBeDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"echo": {
			Name:               "echo",
			Path:               "/bin/echo",
			AllowedPatterns:    []string{"*"},
			RequireCwd:         true,
			AllowedCwdPrefixes: []string{tmpDir},
		},
	}, nil, testCtx)

	missing := broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
		Cwd:     filepath.Join(tmpDir, "missing"),
	})
	if missing.ExitCode != 1 {
		t.Fatalf("missing cwd exit code = %d, want 1", missing.ExitCode)
	}
	if !strings.Contains(missing.Stderr, "cwd does not exist") {
		t.Fatalf("missing cwd stderr = %q", missing.Stderr)
	}

	notDir := broker.Handle(&sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
		Cwd:     filePath,
	})
	if notDir.ExitCode != 1 {
		t.Fatalf("file cwd exit code = %d, want 1", notDir.ExitCode)
	}
	if !strings.Contains(notDir.Stderr, "cwd must be a directory") {
		t.Fatalf("file cwd stderr = %q", notDir.Stderr)
	}
}
