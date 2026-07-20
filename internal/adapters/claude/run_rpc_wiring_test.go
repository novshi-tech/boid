package claude

import (
	"context"
	"encoding/json"
	"fmt"
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
// process IS the "boid" subprocess buildTaskPayloadSessionsCmd execs.
//
// Before calling sandbox.RunBoidShim, it checks BOID_BUILTIN_SHIM the same
// way main.go's shouldRunBoidBuiltinShim gate does — a real "boid" binary
// only routes into RunBoidShim when that env var is set; otherwise it falls
// through to the cobra CLI tree instead (a completely different code path).
// codex review on PR #800 (Minor 1) flagged the first version of this helper
// for skipping that check and calling RunBoidShim unconditionally, which
// would keep this "end-to-end" test green even if
// buildTaskPayloadSessionsCmd's env overlay stopped propagating
// BOID_BUILTIN_SHIM — see TestReadSessionsFromRPC_EndToEnd_MissingBuiltinShimFails.
//
// Once past that gate it runs the real sandbox.RunBoidShim entry point (the
// same function main.go's BOID_BUILTIN_SHIM branch calls), then reproduces
// main.go's stdout/stderr/exit-code handling exactly. This lets the e2e test
// exercise the actual subprocess boundary the adapter crosses in production
// (PATH resolution + a real os/exec.Cmd) without building a separate `boid`
// binary.
func TestMain(m *testing.M) {
	if os.Getenv(boidShimHelperEnv) == "1" {
		if os.Getenv("BOID_BUILTIN_SHIM") == "" {
			os.Stderr.WriteString("boid shim helper: BOID_BUILTIN_SHIM not set; a real boid binary would not route into RunBoidShim here\n")
			os.Exit(2)
		}
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

// startFakeBroker listens on a fresh temp unix socket and, for the single
// ExecRequest it receives, records it on reqCh and replies with sessionsJSON
// — but only when req.Token == wantToken. A mismatched token gets a
// broker-style rejection (ExitCode 1) instead, mirroring (closely enough to
// catch a propagation regression, not to be a full reimplementation of) the
// real broker's own token check in internal/sandbox/broker.go. codex review
// on PR #800 (Minor 1) flagged the original version of this helper for never
// checking the token at all — see
// TestReadSessionsFromRPC_EndToEnd_WrongTokenFails.
func startFakeBroker(t *testing.T, wantToken string, sessionsJSON []byte) (sockPath string, reqCh chan sandbox.ExecRequest) {
	t.Helper()
	dir := t.TempDir()
	sockPath = filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

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
		if req.Token != wantToken {
			_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{
				ExitCode: 1,
				Stderr:   fmt.Sprintf("fake broker: unexpected token %q", req.Token),
			})
			return
		}
		_ = json.NewEncoder(conn).Encode(&sandbox.ExecResponse{Stdout: string(sessionsJSON)})
	}()
	return sockPath, reqCh
}

// installBoidShimHelper points PATH at a "boid" symlink to the current test
// binary (exactly like the shim-bin dir the dispatcher builds in the
// sandbox, docs/plans/phase5-shim-and-task-context.md "5a") and arms
// boidShimHelperEnv so a re-exec of that binary lands in TestMain's helper
// branch above instead of running the go test suite again.
func installBoidShimHelper(t *testing.T) {
	t.Helper()
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
}

// TestReadSessionsFromRPC_EndToEnd proves the claude adapter can fetch prior
// session entries through the real broker + shim path: a fake broker unix
// socket replies to a `task_payload` op exactly like the real broker's
// BoidOpTaskPayload handler would, and the "boid" program on PATH is a real
// subprocess (the re-executed test binary standing in for the sandbox's
// shim-bin `boid`), not an in-process function call.
func TestReadSessionsFromRPC_EndToEnd(t *testing.T) {
	wantSessions := []session{
		{Type: "execution", Name: "", ID: "abc-123"},
		{Type: "execution", Name: "verifier", ID: "def-456"},
	}
	sessionsJSON, err := json.Marshal(wantSessions)
	if err != nil {
		t.Fatal(err)
	}

	const wantToken = "tok"
	sockPath, reqCh := startFakeBroker(t, wantToken, sessionsJSON)
	installBoidShimHelper(t)

	env := map[string]string{
		"BOID_BUILTIN_SHIM":  "1",
		"BOID_TASK_ID":       "t1",
		"BOID_JOB_ID":        "job-1",
		"BOID_BROKER_SOCKET": sockPath,
		"BOID_BROKER_TOKEN":  wantToken,
	}

	got, err := readSessionsFromRPC(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
		if req.Token != wantToken {
			t.Errorf("token = %q, want %q (BOID_BROKER_TOKEN propagation)", req.Token, wantToken)
		}
	default:
		t.Fatal("fake broker never received a request")
	}
}

// TestReadSessionsFromRPC_EndToEnd_MissingBuiltinShimFails pins
// BOID_BUILTIN_SHIM propagation end-to-end: without it, a real "boid" binary
// would not route into RunBoidShim at all (main.go's shouldRunBoidBuiltinShim
// gate) — reproduced here by TestMain's helper branch refusing to call
// RunBoidShim and exiting non-zero instead. readSessionsFromRPC must surface
// that as an error (the codex-review Major fix on PR #800: a fetch failure
// must never collapse to "no sessions").
func TestReadSessionsFromRPC_EndToEnd_MissingBuiltinShimFails(t *testing.T) {
	sockPath, _ := startFakeBroker(t, "tok", []byte(`[]`))
	installBoidShimHelper(t)

	env := map[string]string{
		// BOID_BUILTIN_SHIM intentionally omitted.
		"BOID_TASK_ID":       "t1",
		"BOID_JOB_ID":        "job-1",
		"BOID_BROKER_SOCKET": sockPath,
		"BOID_BROKER_TOKEN":  "tok",
	}

	got, err := readSessionsFromRPC(context.Background(), env)
	if err == nil {
		t.Fatal("expected an error when BOID_BUILTIN_SHIM is not propagated to the boid subprocess")
	}
	if got != nil {
		t.Errorf("got %+v, want nil sessions alongside the error", got)
	}
}

// TestReadSessionsFromRPC_EndToEnd_WrongTokenFails pins BOID_BROKER_TOKEN
// propagation end-to-end: the fake broker rejects any request whose token
// does not match, so a regression that drops BOID_BROKER_TOKEN from
// buildTaskPayloadSessionsCmd's env overlay surfaces as a failing test here
// instead of silently reading (or worse, in a real deployment, potentially
// being rejected by) the broker.
func TestReadSessionsFromRPC_EndToEnd_WrongTokenFails(t *testing.T) {
	sockPath, _ := startFakeBroker(t, "expected-token", []byte(`[]`))
	installBoidShimHelper(t)

	env := map[string]string{
		"BOID_BUILTIN_SHIM":  "1",
		"BOID_TASK_ID":       "t1",
		"BOID_JOB_ID":        "job-1",
		"BOID_BROKER_SOCKET": sockPath,
		"BOID_BROKER_TOKEN":  "wrong-token",
	}

	got, err := readSessionsFromRPC(context.Background(), env)
	if err == nil {
		t.Fatal("expected an error when BOID_BROKER_TOKEN does not match what the broker expects")
	}
	if got != nil {
		t.Errorf("got %+v, want nil sessions alongside the error", got)
	}
}
