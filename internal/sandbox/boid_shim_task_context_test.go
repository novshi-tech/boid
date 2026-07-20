package sandbox_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md): `boid task
// current` / `instructions` / `env` / `payload` — the CLI-side (shim) tests
// for the four new task-context ops. Unlike every other `boid task ...`
// subcommand, these read BOID_TASK_ID / BOID_JOB_ID from the environment
// (never a CLI flag) and support a client-side --format (json|yaml) for
// their full-object (no --field) output.

// newFakeBrokerRecording starts a fake broker socket that replies with resp
// and records the single ExecRequest it receives.
func newFakeBrokerRecording(t *testing.T, resp *sandbox.ExecResponse) (sockPath string, reqCh chan sandbox.ExecRequest) {
	t.Helper()
	dir := t.TempDir()
	sockPath = filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close(); os.Remove(sockPath) })
	reqCh = make(chan sandbox.ExecRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req sandbox.ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		reqCh <- req
		_ = json.NewEncoder(conn).Encode(resp)
	}()
	return sockPath, reqCh
}

func TestRunBoidShim_TaskCurrent_UsesEnvIDs(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `{"id":"t1","title":"hello","status":"executing","behavior":"dev"}`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")
	t.Setenv("BOID_JOB_ID", "job-1")

	resp, err := sandbox.RunBoidShim([]string{"task", "current"})
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
	if req.Boid.Op != sandbox.BoidOpTaskCurrent {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskCurrent)
	}
	if req.Boid.TaskID != "t1" {
		t.Errorf("task id = %q, want t1 (from BOID_TASK_ID)", req.Boid.TaskID)
	}
	if req.Boid.JobID != "job-1" {
		t.Errorf("job id = %q, want job-1 (from BOID_JOB_ID)", req.Boid.JobID)
	}
}

func TestRunBoidShim_TaskCurrent_DefaultFormatIsYAML(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `{"id":"t1","title":"hello","status":"executing","behavior":"dev"}`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")
	t.Setenv("BOID_JOB_ID", "job-1")

	resp, err := sandbox.RunBoidShim([]string{"task", "current"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if strings.Contains(resp.Stdout, "{") {
		t.Errorf("stdout = %q, want YAML (no braces) by default", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "title: hello") {
		t.Errorf("stdout = %q, want YAML rendering of title", resp.Stdout)
	}
}

func TestRunBoidShim_TaskCurrent_FormatJSON(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `{"id":"t1","title":"hello"}`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")
	t.Setenv("BOID_JOB_ID", "job-1")

	resp, err := sandbox.RunBoidShim([]string{"task", "current", "--format", "json"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.Stdout != `{"id":"t1","title":"hello"}` {
		t.Errorf("stdout = %q, want the raw JSON passthrough", resp.Stdout)
	}
}

func TestRunBoidShim_TaskCurrent_FieldSkipsFormatting(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{Stdout: "hello"})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")
	t.Setenv("BOID_JOB_ID", "job-1")

	resp, err := sandbox.RunBoidShim([]string{"task", "current", "--field", "title"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.Stdout != "hello" {
		t.Errorf("stdout = %q, want the plain-text field value unmodified", resp.Stdout)
	}

	req := <-reqCh
	if req.Boid.TaskField != "title" {
		t.Errorf("task_field = %q, want title", req.Boid.TaskField)
	}
}

func TestRunBoidShim_TaskCurrent_InvalidFormatRejected(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_TASK_ID", "t1")

	_, err := sandbox.RunBoidShim([]string{"task", "current", "--format", "xml"})
	if err == nil {
		t.Fatal("expected error for unsupported --format value")
	}
	if !strings.Contains(err.Error(), "--format") {
		t.Errorf("error = %v, want mention of --format", err)
	}
}

func TestRunBoidShim_TaskCurrent_UnsupportedFlagRejected(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", "/tmp/does-not-matter")
	t.Setenv("BOID_TASK_ID", "t1")

	_, err := sandbox.RunBoidShim([]string{"task", "current", "--bogus"})
	if err == nil || !strings.Contains(err.Error(), "unsupported flag") {
		t.Fatalf("expected unsupported flag error, got: %v", err)
	}
}

func TestRunBoidShim_TaskInstructions_UsesEnvIDs(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{Stdout: `[]`})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")
	t.Setenv("BOID_JOB_ID", "job-1")

	resp, err := sandbox.RunBoidShim([]string{"task", "instructions"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid.Op != sandbox.BoidOpTaskInstructions {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskInstructions)
	}
	if req.Boid.TaskID != "t1" || req.Boid.JobID != "job-1" {
		t.Errorf("ids = task:%q job:%q, want t1/job-1", req.Boid.TaskID, req.Boid.JobID)
	}
}

func TestRunBoidShim_TaskEnv_UsesEnvIDs(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		Stdout: `{"allowed_domains":["github.com"],"host_commands":[]}`,
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")
	t.Setenv("BOID_JOB_ID", "job-1")

	resp, err := sandbox.RunBoidShim([]string{"task", "env"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "github.com") {
		t.Errorf("stdout = %q, want the allowed domain rendered", resp.Stdout)
	}

	req := <-reqCh
	if req.Boid.Op != sandbox.BoidOpTaskEnv {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskEnv)
	}
	if req.Boid.JobID != "job-1" {
		t.Errorf("job id = %q, want job-1", req.Boid.JobID)
	}
}

func TestRunBoidShim_TaskPayload_FieldQuery(t *testing.T) {
	sockPath, reqCh := newFakeBrokerRecording(t, &sandbox.ExecResponse{Stdout: `["s1","s2"]`})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_TASK_ID", "t1")
	t.Setenv("BOID_JOB_ID", "job-1")

	resp, err := sandbox.RunBoidShim([]string{"task", "payload", "--field", "artifact.claude_code.sessions"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != `["s1","s2"]` {
		t.Errorf("stdout = %q, want the sessions array unmodified (field mode skips YAML re-render)", resp.Stdout)
	}

	req := <-reqCh
	if req.Boid.Op != sandbox.BoidOpTaskPayload {
		t.Fatalf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskPayload)
	}
	if req.Boid.TaskField != "artifact.claude_code.sessions" {
		t.Errorf("task_field = %q", req.Boid.TaskField)
	}
}

func TestRunBoidShim_TaskEnv_BrokerErrorPropagates(t *testing.T) {
	sockPath, _ := newFakeBrokerRecording(t, &sandbox.ExecResponse{
		ExitCode: 1,
		Stderr:   "boid task env: no context tracked for job \"job-1\"",
	})
	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	t.Setenv("BOID_JOB_ID", "job-1")

	resp, err := sandbox.RunBoidShim([]string{"task", "env"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "no context tracked") {
		t.Errorf("stderr = %q, want the broker's error message passed through", resp.Stderr)
	}
}
