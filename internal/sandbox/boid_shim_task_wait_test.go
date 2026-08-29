package sandbox_test

// Shim-level coverage for `boid task wait <id>` — the CLI surface a trigger's
// `run:` command actually types. Kept separate from the broker/executor tests
// because a gap here is invisible to them: PR #1012's review found exactly that
// shape (an op whose executor chain was fully tested while the shim had no case
// for the flag at all, making the feature unusable from inside a sandbox).

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestRunBoidShim_TaskWait_SendsTypedRequest(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close(); os.Remove(sockPath) })

	reqCh := make(chan sandbox.ExecRequest, 1)
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
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{ExitCode: 0, Stdout: "task round-7: done\n"})
	}()

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "tok-taskwait")

	resp, err := sandbox.RunBoidShim([]string{"task", "wait", "round-7"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	req := <-reqCh
	if req.Boid == nil {
		t.Fatal("expected a boid request")
	}
	if req.Boid.Op != sandbox.BoidOpTaskWait {
		t.Errorf("op = %q, want task_wait", req.Boid.Op)
	}
	if req.Boid.TaskID != "round-7" {
		t.Errorf("task id = %q, want round-7", req.Boid.TaskID)
	}
}

// Parse errors surface before the shim dials, so these need the broker env set
// (RunBoidShim checks for it first) but no listener behind it.
func TestRunBoidShim_TaskWait_RequiresATaskID(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	if _, err := sandbox.RunBoidShim([]string{"task", "wait"}); err == nil {
		t.Fatal("expected an error for `boid task wait` with no id")
	} else if !strings.Contains(err.Error(), "requires a task id") {
		t.Errorf("err = %v, want the missing-id message", err)
	}
}

// The op takes no flags on purpose (see parseBoidTaskWait's doc comment): a
// per-call --timeout would be a duration the daemon does not know about, which
// is the exact split that put `timeout 300` inside a workspace's own `run:`
// string. Rejecting flags outright keeps that from being reintroduced quietly.
func TestRunBoidShim_TaskWait_RejectsFlags(t *testing.T) {
	t.Setenv("BOID_BROKER_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	t.Setenv("BOID_BROKER_TOKEN", "tok")
	if _, err := sandbox.RunBoidShim([]string{"task", "wait", "round-7", "--timeout", "30m"}); err == nil {
		t.Fatal("expected an error for an unsupported flag")
	}
}
