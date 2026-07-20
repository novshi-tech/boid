package claude

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR3 (docs/plans/phase5-shim-and-task-context.md) "e2e wiring"
// layer, mirroring the 5b-1 pattern (adapter unit / broker exec / e2e
// wiring — see internal/sandbox's boid_shim_task_context_test.go and
// broker_task_context_test.go for the other two layers, unchanged by this
// PR).
//
// The adapter-unit tests in run_test.go stub fetchTaskPayloadSessions, so
// they never prove that a real "boid" subprocess is reachable on PATH, that
// buildTaskPayloadSessionsCmd's env overlay actually lands in that
// subprocess's environment, or that the boid shim's --field/JSON passthrough
// round-trips correctly into readSessionsFromRPC's JSON parsing. This file
// closes that gap by re-executing the compiled test binary itself as the
// "boid" program the adapter execs (a standard Go testing idiom — see
// os/exec_test.go's TestHelperProcess pattern in the Go standard library),
// backed by a fake broker unix socket that behaves exactly like
// boid_shim_task_context_test.go's newFakeBrokerRecording.

// boidShimHelperEnv, when set to "1" in a re-executed copy of this test
// binary, makes TestMain below become the "boid" program instead of running
// the package's go test suite. See TestReadSessionsFromRPC_EndToEnd.
const boidShimHelperEnv = "BOID_TEST_SHIM_HELPER"

// TestMain intercepts the compiled test binary re-exec used by
// TestReadSessionsFromRPC_EndToEnd: when boidShimHelperEnv=1 is set, this
// process IS the "boid" subprocess buildTaskPayloadSessionsCmd execs — it
// runs the real sandbox.RunBoidShim entry point (the same function main.go's
// BOID_BUILTIN_SHIM branch calls) instead of the normal `go test` runner,
// then reproduces main.go's stdout/stderr/exit-code handling exactly. This
// lets the e2e test exercise the actual subprocess boundary the adapter
// crosses in production (PATH resolution + a real os/exec.Cmd) without
// building a separate `boid` binary.
func TestMain(m *testing.M) {
	if os.Getenv(boidShimHelperEnv) == "1" {
		resp, err := sandbox.RunBoidShim(os.Args[1:])
		if err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		if resp.Stdout != "" {
			os.Stdout.WriteString(resp.Stdout)
		}
		if resp.Stderr != "" {
			os.Stderr.WriteString(resp.Stderr)
		}
		os.Exit(resp.ExitCode)
	}
	os.Exit(m.Run())
}

// TestReadSessionsFromRPC_EndToEnd proves the claude adapter can fetch prior
// session entries through the real broker + shim path: a fake broker unix
// socket replies to a `task_payload` op exactly like the real broker's
// BoidOpTaskPayload handler would, and the "boid" program on PATH is a real
// subprocess (the re-executed test binary standing in for the sandbox's
// shim-bin `boid`), not an in-process function call.
func TestReadSessionsFromRPC_EndToEnd(t *testing.T) {
	// 1. Fake broker: decode one ExecRequest, reply with the canned sessions
	// JSON api.ResolveJSONField would produce for
	// --field artifact.claude_code.sessions.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	wantSessions := []session{
		{Type: "execution", Name: "", ID: "abc-123"},
		{Type: "execution", Name: "verifier", ID: "def-456"},
	}
	sessionsJSON, err := json.Marshal(wantSessions)
	if err != nil {
		t.Fatal(err)
	}

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
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{
			Stdout: string(sessionsJSON),
		})
	}()

	// 2. Point PATH at a "boid" symlink to the current test binary, exactly
	// like the shim-bin dir the dispatcher builds in the sandbox
	// (docs/plans/phase5-shim-and-task-context.md "5a").
	selfExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binDir := t.TempDir()
	boidPath := filepath.Join(binDir, "boid")
	if err := os.Symlink(selfExe, boidPath); err != nil {
		t.Fatalf("symlink boid helper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(boidShimHelperEnv, "1")

	env := map[string]string{
		"BOID_BUILTIN_SHIM":  "1",
		"BOID_TASK_ID":       "t1",
		"BOID_JOB_ID":        "job-1",
		"BOID_BROKER_SOCKET": sockPath,
		"BOID_BROKER_TOKEN":  "tok",
	}

	got := readSessionsFromRPC(context.Background(), env)
	if !reflect.DeepEqual(got, wantSessions) {
		t.Fatalf("got %+v, want %+v", got, wantSessions)
	}

	select {
	case req := <-reqCh:
		if req.Boid == nil {
			t.Fatal("expected typed boid request")
		}
		if req.Boid.Op != sandbox.BoidOpTaskPayload {
			t.Errorf("op = %q, want %q", req.Boid.Op, sandbox.BoidOpTaskPayload)
		}
		if req.Boid.TaskField != sessionsFieldPath {
			t.Errorf("task_field = %q, want %q", req.Boid.TaskField, sessionsFieldPath)
		}
		if req.Boid.TaskID != "t1" || req.Boid.JobID != "job-1" {
			t.Errorf("ids = task:%q job:%q, want t1/job-1", req.Boid.TaskID, req.Boid.JobID)
		}
	default:
		t.Fatal("fake broker never received a request")
	}
}
