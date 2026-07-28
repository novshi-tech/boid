package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
)

// runJobLogAgainst points `boid job log` at a stub daemon that answers the
// log endpoint with the given status and body, and returns what the command
// printed.
//
// The command objects are package-level singletons, so the context and the
// output sinks are restored on cleanup — the same discipline
// newInitScriptCmd (workspace_init_script_test.go) documents for the
// workspace commands.
func runJobLogAgainst(t *testing.T, status int, body string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build a client for %s: %v", srv.URL, err)
	}

	cmd := jobLogCmd
	prev := cmd.Context()
	t.Cleanup(func() {
		cmd.SetContext(prev)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), c))

	if err := runJobLog(cmd, []string{"some-id"}); err != nil {
		t.Fatalf("runJobLog: %v", err)
	}
	return out.String()
}

// TestJobLog_404BodyIsShownVerbatim pins that the CLI relays the daemon's
// own explanation instead of substituting its own.
//
// `boid job log` used to print the fixed string "log not available (runtime
// cleaned up)" for EVERY 404, discarding the body — so a mistyped id, a job
// whose sandbox never started, and a genuinely swept transcript were
// indistinguishable at the terminal, and two of the three were reported as
// something that never happened. The daemon now says which one it is
// (internal/api/job.go's writeJobLogUnavailable); this is the half that
// makes an operator actually see it.
func TestJobLog_404BodyIsShownVerbatim(t *testing.T) {
	const daemonSaid = `no such job "some-id" — if this is a task id, list that task's jobs first: boid job list --task some-id`

	out := runJobLogAgainst(t, http.StatusNotFound, daemonSaid+"\n")

	if !strings.Contains(out, daemonSaid) {
		t.Errorf("output = %q, want it to carry the daemon's own message %q", out, daemonSaid)
	}
	if strings.Contains(out, "runtime cleaned up") {
		t.Errorf("output = %q must not substitute the old fixed 'runtime cleaned up' text for whatever the daemon said", out)
	}
}

// TestJobLog_TranscriptIsPrintedUnchanged guards the success path against
// collateral damage from the 404 change: a transcript is raw terminal output
// (escape sequences included) and must reach stdout byte for byte.
func TestJobLog_TranscriptIsPrintedUnchanged(t *testing.T) {
	const transcript = "\x1b[1mhello\x1b[0m\nsecond line\n"

	out := runJobLogAgainst(t, http.StatusOK, transcript)

	if out != transcript {
		t.Errorf("output = %q, want the transcript verbatim %q", out, transcript)
	}
}

// TestJobLog_ServerErrorStillFails pins that non-404 failures stay failures:
// relaying the body is specific to 404, and a 500 must not be printed as if
// it were a log.
func TestJobLog_ServerErrorStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"disk error"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build a client for %s: %v", srv.URL, err)
	}

	cmd := jobLogCmd
	prev := cmd.Context()
	t.Cleanup(func() {
		cmd.SetContext(prev)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(client.WithClient(context.Background(), c))

	err = runJobLog(cmd, []string{"some-id"})
	if err == nil {
		t.Fatal("runJobLog returned nil for a 500; a server error must not look like a successful log read")
	}
	if !strings.Contains(err.Error(), "disk error") {
		t.Errorf("error = %v, want it to carry the server's message", err)
	}
}
