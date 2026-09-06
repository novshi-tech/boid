package sandbox_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// `boid card context` CLI-side (shim) tests. Unlike `boid task current`/etc,
// this takes NO caller-supplied id at all (not even from an env var):
// identity comes exclusively from the broker token. Shares the
// --field/--format output convention with the task-context subcommands
// (boid_shim_task_context_test.go).

func TestRunBoidShim_CardContext_NoIDsSent(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `{"card_id":"card-1","request_id":"req-1","command_key":"review","instruction":"do the thing","origin":"human"}`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TLS_ADDR", "")
	t.Setenv("BOID_BROKER_TOKEN", "tok")

	resp, err := sandbox.RunBoidShim([]string{"card", "context"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected typed boid request")
	}
	if req.Boid.Op != sandbox.BoidOpCardContext {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpCardContext)
	}
	if req.Boid.TaskID != "" || req.Boid.JobID != "" {
		t.Errorf("expected no id fields sent (identity is token-only), got TaskID=%q JobID=%q", req.Boid.TaskID, req.Boid.JobID)
	}
}

func TestRunBoidShim_CardContext_DefaultFormatIsYAML(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `{"card_id":"card-1","request_id":"req-1","command_key":"review","instruction":"do the thing","origin":"human"}`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TLS_ADDR", "")
	t.Setenv("BOID_BROKER_TOKEN", "tok")

	resp, err := sandbox.RunBoidShim([]string{"card", "context"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if strings.Contains(resp.Stdout, "{") {
		t.Errorf("expected YAML output by default, got JSON-looking stdout: %q", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "card_id: card-1") {
		t.Errorf("expected YAML rendering of card_id, got: %q", resp.Stdout)
	}
}

func TestRunBoidShim_CardContext_FormatJSON(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `{"card_id":"card-1","request_id":"req-1","command_key":"review","instruction":"do the thing","origin":"human"}`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TLS_ADDR", "")
	t.Setenv("BOID_BROKER_TOKEN", "tok")

	resp, err := sandbox.RunBoidShim([]string{"card", "context", "--format", "json"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, `"card_id":"card-1"`) {
		t.Errorf("expected raw JSON passthrough, got: %q", resp.Stdout)
	}
}

func TestRunBoidShim_CardContext_Field(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{Stdout: "do the thing"})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TLS_ADDR", "")
	t.Setenv("BOID_BROKER_TOKEN", "tok")

	resp, err := sandbox.RunBoidShim([]string{"card", "context", "--field", "instruction"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "do the thing" {
		t.Errorf("Stdout = %q, want %q (unrendered scalar)", resp.Stdout, "do the thing")
	}

	req := <-reqCh
	if req.Boid.TaskField != "instruction" {
		t.Errorf("TaskField = %q, want %q", req.Boid.TaskField, "instruction")
	}
}

func TestRunBoidShim_CardContext_ClearErrorPassesThrough(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		ExitCode: 1,
		Stderr:   "boid card context: no card context for this job",
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TLS_ADDR", "")
	t.Setenv("BOID_BROKER_TOKEN", "tok")

	resp, err := sandbox.RunBoidShim([]string{"card", "context"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "no card context") {
		t.Fatalf("expected the broker's clear error to pass through unrendered, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}
