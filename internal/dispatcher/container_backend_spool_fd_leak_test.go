package dispatcher

import (
	"context"
	"os"
	"testing"

	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins [Major 10, PR7 codex review]: a Launch error path that
// runs AFTER the transcript spool was already successfully opened
// (openTranscriptSpool, container_backend_transcript_spool_failure_test.go
// — Major 8) must close it, or every retry of a persistently-failing
// dispatch (e.g. an OCI config error rejected at attach/start) leaks one fd.

// openFDCount returns the number of open file descriptors this process
// currently holds, via /proc/self/fd (Linux-only — CLAUDE.md「Linux のみ
// 対応」, so this is safe to rely on directly rather than gated behind a
// build tag). Used by the fd-leak regression tests below to prove a Launch
// error path actually closes the transcript spool file it opened, rather
// than merely asserting on log output or file existence (neither of which
// can distinguish "closed" from "open and leaked").
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// TestContainerBackend_Launch_AttachFailure_ClosesTranscriptSpool pins
// [Major 10]: an attach failure after the transcript spool was already
// successfully opened must close that spool file — the pre-fix code
// returned without ever closing it, leaking one fd per failed Launch attempt
// (a real failure mode under a persistently-erroring OCI config: "1
// dispatch = 1 fd leak" until the daemon's fd limit is exhausted). Verified
// via an actual /proc/self/fd count delta, not just log/behavior
// inference, so a regression that reintroduces the leak fails this test
// even if every other assertion still passes.
func TestContainerBackend_Launch_AttachFailure_ClosesTranscriptSpool(t *testing.T) {
	runtimeDir := t.TempDir()
	api := &fakeDockerAPI{
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			return client.ContainerAttachResult{}, context.DeadlineExceeded
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir})

	before := openFDCount(t)
	_, err := be.Launch(context.Background(), sandbox.Spec{ID: "job-attach-fail", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-attach-fail"})
	if err == nil {
		t.Fatal("Launch() = nil error, want the injected attach failure")
	}
	after := openFDCount(t)

	if after > before {
		t.Errorf("open fd count went from %d to %d after a failed Launch, want no net increase (transcript spool fd leaked)", before, after)
	}
}

// TestContainerBackend_Launch_StartFailure_ClosesTranscriptSpool is the
// ContainerStart sibling of the attach-failure test above — same [Major 10]
// fd-leak fix, different Launch error branch.
func TestContainerBackend_Launch_StartFailure_ClosesTranscriptSpool(t *testing.T) {
	runtimeDir := t.TempDir()
	api := &fakeDockerAPI{
		ContainerStartFunc: func(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error) {
			return client.ContainerStartResult{}, context.DeadlineExceeded
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir})

	before := openFDCount(t)
	_, err := be.Launch(context.Background(), sandbox.Spec{ID: "job-start-fail", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-start-fail"})
	if err == nil {
		t.Fatal("Launch() = nil error, want the injected start failure")
	}
	after := openFDCount(t)

	if after > before {
		t.Errorf("open fd count went from %d to %d after a failed Launch, want no net increase (transcript spool fd leaked)", before, after)
	}
}
