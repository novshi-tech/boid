package sandbox

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docs/plans/signal-ingest-detailed-design.md §3.2 (PR-3): boid_shim.go's
// `boid signal list/ack/ingest/cursor` parsing.

func TestParseBoidRequest_Signal_DispatchesToSubcommands(t *testing.T) {
	req, err := parseBoidRequest([]string{"signal", "list", "--claim"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpSignalList {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpSignalList)
	}
	if !req.Claim {
		t.Fatal("expected Claim=true")
	}

	req, err = parseBoidRequest([]string{"signal", "claim", "sig-1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpSignalClaim {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpSignalClaim)
	}

	req, err = parseBoidRequest([]string{"signal", "ack", "sig-1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpSignalAck {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpSignalAck)
	}

	if _, err := parseBoidRequest([]string{"signal", "bogus"}); err == nil {
		t.Fatal("unsupported signal subcommand: expected an error, got success")
	}

	if _, err := parseBoidRequest([]string{"signal"}); err == nil {
		t.Fatal("missing signal subcommand: expected an error, got success")
	}
}

func TestParseBoidSignalList_Defaults(t *testing.T) {
	req, err := parseBoidSignalList(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpSignalList {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpSignalList)
	}
	if req.Claim || req.Connector != "" || req.SignalState != "" || req.Limit != 0 {
		t.Fatalf("expected zero-value defaults, got %+v", req)
	}
	// WorkspaceID must never be set by the shim — it's broker-injected.
	if req.WorkspaceID != "" {
		t.Fatalf("WorkspaceID = %q, want empty (no --workspace-id flag exists for this op)", req.WorkspaceID)
	}
}

func TestParseBoidSignalList_AllFlags(t *testing.T) {
	req, err := parseBoidSignalList([]string{"--claim", "--source", "slack/mentions", "--state", "dead", "--limit", "25", "--json"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !req.Claim {
		t.Error("Claim = false, want true")
	}
	if req.Connector != "slack/mentions" {
		t.Errorf("Connector = %q, want slack/mentions", req.Connector)
	}
	if req.SignalState != "dead" {
		t.Errorf("SignalState = %q, want dead", req.SignalState)
	}
	if req.Limit != 25 {
		t.Errorf("Limit = %d, want 25", req.Limit)
	}
}

func TestParseBoidSignalList_EqualsForm(t *testing.T) {
	req, err := parseBoidSignalList([]string{"--source=jira-cloud/jira-cloud", "--state=acked", "--limit=5"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Connector != "jira-cloud/jira-cloud" || req.SignalState != "acked" || req.Limit != 5 {
		t.Fatalf("unexpected parse result: %+v", req)
	}
}

func TestParseBoidSignalList_Rejects(t *testing.T) {
	cases := map[string][]string{
		"unknown flag":     {"--urgency", "now"},
		"invalid limit":    {"--limit", "abc"},
		"missing --source": {"--source"},
		"positional arg":   {"sig-1"},
	}
	for name, args := range cases {
		if _, err := parseBoidSignalList(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}

// `signal claim` takes positional ids and nothing else — the whole point of
// the op is that the caller names the rows it is handing to a judgment, so
// there is no flag that could select rows on its behalf (that is what the
// deprecated `list --claim` did, and why it could not tell "read" from
// "handed to a judgment").
func TestParseBoidSignalClaim(t *testing.T) {
	req, err := parseBoidSignalClaim([]string{"sig-1", "sig-2"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpSignalClaim {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpSignalClaim)
	}
	want := []string{"sig-1", "sig-2"}
	if len(req.SignalIDs) != len(want) {
		t.Fatalf("SignalIDs = %v, want %v", req.SignalIDs, want)
	}
	for i, id := range want {
		if req.SignalIDs[i] != id {
			t.Errorf("SignalIDs[%d] = %q, want %q", i, req.SignalIDs[i], id)
		}
	}
}

func TestParseBoidSignalClaim_Rejects(t *testing.T) {
	cases := map[string][]string{
		"no ids":     {},
		"stray flag": {"--limit", "3"},
	}
	for name, args := range cases {
		if _, err := parseBoidSignalClaim(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}

func TestParseBoidSignalAck(t *testing.T) {
	req, err := parseBoidSignalAck([]string{"sig-1", "sig-2", "sig-3"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpSignalAck {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpSignalAck)
	}
	want := []string{"sig-1", "sig-2", "sig-3"}
	if len(req.SignalIDs) != len(want) {
		t.Fatalf("SignalIDs = %v, want %v", req.SignalIDs, want)
	}
	for i, id := range want {
		if req.SignalIDs[i] != id {
			t.Errorf("SignalIDs[%d] = %q, want %q", i, req.SignalIDs[i], id)
		}
	}
}

func TestParseBoidSignalAck_Rejects(t *testing.T) {
	cases := map[string][]string{
		"no ids":     {},
		"stray flag": {"--force", "sig-1"},
	}
	for name, args := range cases {
		if _, err := parseBoidSignalAck(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}

func TestParseBoidSignalIngest_RequiresEnv(t *testing.T) {
	if _, err := parseBoidSignalIngest(nil); err == nil {
		t.Fatal("expected an error when BOID_SIGNAL_SERVICE/BOID_SIGNAL_CONNECTOR are unset")
	}
}

func TestParseBoidSignalIngest_RejectsArguments(t *testing.T) {
	t.Setenv("BOID_SIGNAL_SERVICE", "svc")
	t.Setenv("BOID_SIGNAL_CONNECTOR", "pack/conn")
	if _, err := parseBoidSignalIngest([]string{"unexpected"}); err == nil {
		t.Fatal("expected an error for a positional argument")
	}
}

func TestParseBoidSignalIngest_ReadsStdinAndEnv(t *testing.T) {
	t.Setenv("BOID_SIGNAL_SERVICE", "jira-api")
	t.Setenv("BOID_SIGNAL_CONNECTOR", "jira-cloud/jira-cloud")

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
	body := `{"id":"1","occurred_at":"2026-08-26T00:00:00Z","identity":"jira:X-1"}` + "\n"
	go func() {
		_, _ = w.WriteString(body)
		w.Close()
	}()

	req, err := parseBoidSignalIngest(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpSignalIngest {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpSignalIngest)
	}
	if req.Service != "jira-api" || req.Connector != "jira-cloud/jira-cloud" {
		t.Fatalf("service/connector = %q/%q, want jira-api/jira-cloud/jira-cloud", req.Service, req.Connector)
	}
	if string(req.IngestPayload) != body {
		t.Fatalf("IngestPayload = %q, want %q", req.IngestPayload, body)
	}
}

// TestParseBoidSignalIngest_RejectsOversizedStdin pins the shim-side half of
// the 10 MiB cap (design doc §3.2: "既存 PayloadPatchMaxBytes と同値") —
// mirrors TestRunBoidShim_TaskUpdatePayloadPatch_RejectsOversizedContent's
// approach of writing exactly cap+1 bytes rather than an arbitrarily large
// amount.
func TestParseBoidSignalIngest_RejectsOversizedStdin(t *testing.T) {
	t.Setenv("BOID_SIGNAL_SERVICE", "svc")
	t.Setenv("BOID_SIGNAL_CONNECTOR", "pack/conn")

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	oversized := make([]byte, PayloadPatchMaxBytes+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	go func() {
		_, _ = w.Write(oversized)
		w.Close()
	}()

	if _, err := parseBoidSignalIngest(nil); err == nil {
		t.Fatal("expected an error for stdin content exceeding PayloadPatchMaxBytes")
	}
}

func TestParseBoidSignalCursor_RequiresEnv(t *testing.T) {
	if _, err := parseBoidSignalCursor(nil); err == nil {
		t.Fatal("expected an error when BOID_SIGNAL_SERVICE/BOID_SIGNAL_CONNECTOR are unset")
	}
}

func TestParseBoidSignalCursor_ReadsEnv(t *testing.T) {
	t.Setenv("BOID_SIGNAL_SERVICE", "slack-api")
	t.Setenv("BOID_SIGNAL_CONNECTOR", "slack/mentions")

	req, err := parseBoidSignalCursor(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpSignalCursorGet {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpSignalCursorGet)
	}
	if req.Service != "slack-api" || req.Connector != "slack/mentions" {
		t.Fatalf("service/connector = %q/%q, want slack-api/slack/mentions", req.Service, req.Connector)
	}
}

func TestParseBoidSignalCursor_RejectsArguments(t *testing.T) {
	t.Setenv("BOID_SIGNAL_SERVICE", "svc")
	t.Setenv("BOID_SIGNAL_CONNECTOR", "pack/conn")
	if _, err := parseBoidSignalCursor([]string{"unexpected"}); err == nil {
		t.Fatal("expected an error for a positional argument")
	}
}

// TestRunBoidShim_SignalAck_TwiceIsHarmless drives `boid signal ack sig-1`
// through the FULL shim entry point (RunBoidShim, not just the parse
// function) twice against a fake broker socket, closing the "shim 経由" leg
// of Q14's shim/broker/executor idempotency chain — the fake broker always
// replies success, so this specifically pins that the shim itself adds no
// state or rejection across repeated calls with the same id.
func TestRunBoidShim_SignalAck_TwiceIsHarmless(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	reqCh := make(chan ExecRequest, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req ExecRequest
			if err := json.NewDecoder(conn).Decode(&req); err == nil {
				reqCh <- req
			}
			_ = json.NewEncoder(conn).Encode(&ExecResponse{ExitCode: 0})
			conn.Close()
		}
	}()

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-signal")

	for i := 0; i < 2; i++ {
		resp, err := RunBoidShim([]string{"signal", "ack", "sig-1"})
		if err != nil {
			t.Fatalf("call %d: RunBoidShim: %v", i, err)
		}
		if resp.ExitCode != 0 {
			t.Fatalf("call %d: exit code = %d, want 0", i, resp.ExitCode)
		}
	}

	for i := 0; i < 2; i++ {
		req := <-reqCh
		if req.Boid == nil || req.Boid.Op != BoidOpSignalAck {
			t.Fatalf("call %d: unexpected request %+v", i, req)
		}
		if len(req.Boid.SignalIDs) != 1 || req.Boid.SignalIDs[0] != "sig-1" {
			t.Fatalf("call %d: SignalIDs = %v, want [sig-1]", i, req.Boid.SignalIDs)
		}
	}
}

func TestRunBoidShim_SignalList_Claim(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "broker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	reqCh := make(chan ExecRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req ExecRequest
		if err := json.NewDecoder(conn).Decode(&req); err == nil {
			reqCh <- req
		}
		_ = json.NewEncoder(conn).Encode(&ExecResponse{ExitCode: 0, Stdout: "[]\n"})
	}()

	t.Setenv("BOID_BROKER_SOCKET", sockPath)
	t.Setenv("BOID_BROKER_TOKEN", "token-signal")

	resp, err := RunBoidShim([]string{"signal", "list", "--claim", "--limit", "10"})
	if err != nil {
		t.Fatalf("RunBoidShim: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}
	if !strings.Contains(resp.Stdout, "[]") {
		t.Fatalf("stdout = %q, want the fake broker's JSON body", resp.Stdout)
	}

	req := <-reqCh
	if req.Boid == nil || req.Boid.Op != BoidOpSignalList || !req.Boid.Claim || req.Boid.Limit != 10 {
		t.Fatalf("unexpected request: %+v", req.Boid)
	}
}
