package cmd

// Tests for `boid agent <harness> --no-attach --output json`:
// machine-readable stdout for --no-attach, alongside the existing
// stderr-only `job_id=...` line kept unchanged for callers that don't ask
// for --output json.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
)

// captureStderr mirrors captureStdout (observe_test.go) but for os.Stderr,
// which runAgentSession writes its plain-text --no-attach line to directly.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, rerr := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	return string(buf)
}

// startSessionStubServer answers /api/projects/{id}/sessions with a fixed
// job id, and GET /api/projects/{id} (resolveProjectRef) with a minimal
// project — enough for runAgentSession to reach its --no-attach branch
// without a real daemon.
func startSessionStubServer(t *testing.T, jobID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/proj-1":
			json.NewEncoder(w).Encode(map[string]any{"id": "proj-1", "name": "proj-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/proj-1/sessions":
			json.NewEncoder(w).Encode(apiwire.StartSessionResult{JobID: jobID, AttachURL: "/jobs/" + jobID})
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runAgentSessionAgainst(t *testing.T, srv *httptest.Server, flags *agentSessionFlags) error {
	t.Helper()
	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	ctx := client.WithClient(context.Background(), c)
	return runAgentSession(ctx, "claude", flags)
}

func TestRunAgentSession_NoAttach_OutputJSON_PrintsMachineReadableStdout(t *testing.T) {
	srv := startSessionStubServer(t, "job-abc")
	flags := &agentSessionFlags{projectRef: "proj-1", noAttach: true, output: "json"}

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runAgentSessionAgainst(t, srv, flags)
	})
	if runErr != nil {
		t.Fatalf("runAgentSession: %v", runErr)
	}

	var got struct {
		Kind  string `json:"kind"`
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal stdout %q: %v", stdout, err)
	}
	if got.Kind != "session" || got.JobID != "job-abc" {
		t.Errorf("got %+v, want kind=session job_id=job-abc", got)
	}
}

func TestRunAgentSession_NoAttach_DefaultOutput_StderrJobIDLineUnchanged(t *testing.T) {
	srv := startSessionStubServer(t, "job-xyz")
	flags := &agentSessionFlags{projectRef: "proj-1", noAttach: true}

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runAgentSessionAgainst(t, srv, flags)
	})
	if runErr != nil {
		t.Fatalf("runAgentSession: %v", runErr)
	}
	if stderr != "job_id=job-xyz\n" {
		t.Errorf("stderr = %q, want the existing plain job_id= line unchanged", stderr)
	}
}

func TestRunAgentSession_OutputJSON_WithoutNoAttach_Rejected(t *testing.T) {
	srv := startSessionStubServer(t, "job-abc")
	flags := &agentSessionFlags{projectRef: "proj-1", output: "json"}

	err := runAgentSessionAgainst(t, srv, flags)
	if err == nil {
		t.Fatal("expected an error when --output json is given without --no-attach")
	}
}

func TestRunAgentSession_InvalidOutputValue_Rejected(t *testing.T) {
	srv := startSessionStubServer(t, "job-abc")
	flags := &agentSessionFlags{projectRef: "proj-1", noAttach: true, output: "xml"}

	err := runAgentSessionAgainst(t, srv, flags)
	if err == nil {
		t.Fatal("expected an error for an unsupported --output value")
	}
}
