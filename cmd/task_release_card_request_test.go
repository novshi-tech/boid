package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
)

// TestTaskReleaseCardRequest_NoNotice pins the quiet path: a plain release
// with no operator_notice in the response prints only the release
// confirmation line, no warning.
func TestTaskReleaseCardRequest_NoNotice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"released"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := taskReleaseCardRequestCmd
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

	if err := cmd.RunE(cmd, []string{"req-1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if bytes.Contains(out.Bytes(), []byte("warning:")) {
		t.Errorf("output = %s, want no warning line", out.String())
	}
}

// TestTaskReleaseCardRequest_LaunchingRow_PrintsWarningWithLauncherJobID
// pins the case this PR item exists for: force-releasing a "launching" row
// (no continuation attached yet) must still print a warning, and that
// warning must surface launcher_job_id so the operator can trace the job
// with `boid job` — previously only an already-attached row produced any
// warning at all, and a stuck launching row released silently.
func TestTaskReleaseCardRequest_LaunchingRow_PrintsWarningWithLauncherJobID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"released","launcher_job_id":"job-99","operator_notice":"launcher job job-99 is NOT stopped"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := taskReleaseCardRequestCmd
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

	if err := cmd.RunE(cmd, []string{"req-1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	got := out.String()
	if !bytes.Contains(out.Bytes(), []byte("warning:")) {
		t.Errorf("output = %s, want a warning line for a stuck launching row", got)
	}
	if !bytes.Contains(out.Bytes(), []byte("job-99")) {
		t.Errorf("output = %s, want launcher_job_id job-99 surfaced so the operator can trace it", got)
	}
}

// TestTaskReleaseCardRequest_FoldedSiblingsFailed_PrintedIndependentlyOfNotice
// pins that folded_siblings_failed lines print whether or not
// operator_notice is also set — the CLI must not assume the two always
// travel together.
func TestTaskReleaseCardRequest_FoldedSiblingsFailed_PrintedIndependentlyOfNotice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"released","folded_siblings_failed":[{"id":"req-2","command_key":"deploy"},{"id":"req-3","command_key":"lint"}]}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := taskReleaseCardRequestCmd
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

	if err := cmd.RunE(cmd, []string{"req-1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	got := out.String()
	if bytes.Contains(out.Bytes(), []byte("warning:")) {
		t.Errorf("output = %s, want no warning line (operator_notice was empty)", got)
	}
	if !bytes.Contains(out.Bytes(), []byte("req-2")) || !bytes.Contains(out.Bytes(), []byte("deploy")) {
		t.Errorf("output = %s, want folded sibling req-2/deploy printed", got)
	}
	if !bytes.Contains(out.Bytes(), []byte("req-3")) || !bytes.Contains(out.Bytes(), []byte("lint")) {
		t.Errorf("output = %s, want folded sibling req-3/lint printed", got)
	}
}

// TestTaskReleaseCardRequest_AttachedTarget_PrintsWarning pins the
// already-covered case still works after the shared api.ReleaseResult type
// replaced this command's own duplicated struct.
func TestTaskReleaseCardRequest_AttachedTarget_PrintsWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"released","target_kind":"task","target_id":"task-1","had_attached_target":true,"operator_notice":"the task task-1 it was attached to is NOT stopped"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	cmd := taskReleaseCardRequestCmd
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

	if err := cmd.RunE(cmd, []string{"req-1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if !bytes.Contains(out.Bytes(), []byte("warning:")) {
		t.Errorf("output = %s, want a warning line for an attached target", out.String())
	}
}
