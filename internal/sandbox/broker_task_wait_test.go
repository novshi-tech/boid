package sandbox_test

// Broker-side validation for task_wait. The policy gate itself is covered by
// TestBroker_BoidTaskWait_PolicyReject (broker_op_escape_test.go); what these
// pin is the one thing task_wait deliberately does DIFFERENTLY from its sibling
// blocking op, task_ask.

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func handleTaskWait(t *testing.T, ctx sandbox.TokenContext, req *sandbox.BoidRequest) (*sandbox.ExecResponse, *fakeBoidExecutor) {
	t.Helper()
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx.Role = testRoleHook
	ctx.ProjectDir = projectDir
	policies := map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{string(sandbox.BoidOpTaskWait): {}}},
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, policies, ctx)
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    req,
	})
	return resp, exec
}

// task_ask fills an omitted TaskID from the token context, because it always
// targets the caller's own task. task_wait must NOT: waiting on yourself never
// returns, so an omitted id is a mistake to name rather than a default to
// supply. Pinning it here because the two ops sit next to each other in the
// broker's switch and the ask-style default is the easy thing to copy.
func TestBroker_BoidTaskWait_EmptyTaskIDIsNotDefaultedFromContext(t *testing.T) {
	resp, exec := handleTaskWait(t,
		sandbox.TokenContext{TaskID: "caller-task"},
		&sandbox.BoidRequest{Op: sandbox.BoidOpTaskWait},
	)

	if resp.ExitCode == 0 {
		t.Fatal("task_wait without a task id should be rejected")
	}
	if !strings.Contains(resp.Stderr, "requires a task id") {
		t.Errorf("stderr = %q, want the missing-id rejection", resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor must not be reached, got %d calls", len(exec.calls))
	}
}

// A well-formed request reaches the executor with the id the caller named,
// untouched — the broker validates here and leaves project authorization to the
// executor, the same split every other task op uses.
func TestBroker_BoidTaskWait_PassesTaskIDThrough(t *testing.T) {
	resp, exec := handleTaskWait(t,
		sandbox.TokenContext{TaskID: "caller-task"},
		&sandbox.BoidRequest{Op: sandbox.BoidOpTaskWait, TaskID: "round-7"},
	)

	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if got := exec.calls[0].TaskID; got != "round-7" {
		t.Errorf("executor saw task id %q, want round-7", got)
	}
}

// The seam that makes an abandoned wait recoverable: handleConn only gives a
// request a connection-scoped context when isBlockingBoidRequest says so, and
// that is an explicit per-op list. Dropped from it, task_wait still blocks —
// with nothing left to unblock it but daemon shutdown, and no symptom at the
// call site. This is the task_wait twin of
// TestBroker_TaskAsk_ConnCloseCancelsContext; both ops that block need one,
// which is what keeps the list honest.
func TestBroker_TaskWait_ConnCloseCancelsContext(t *testing.T) {
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
		"boid": {AllowedOps: map[string]struct{}{string(sandbox.BoidOpTaskWait): {}}},
	}
	token := broker.Register(nil, policies, sandbox.TokenContext{
		JobID:      "j1",
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
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskWait, TaskID: "round-7"},
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
		t.Fatal("the wait was not cancelled when the connection closed — is task_wait still listed in isBlockingBoidRequest?")
	}
}
