package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins Blocker 1 from the PR7 codex review
// (docs/plans/phase6-container-backend.md §PR7 cutover): a compose deploy
// runs the daemon inside its own container, so a bind-mount Source it hands
// the sibling docker daemon (DooD) must be a HOST-visible path — the
// daemon's own private /tmp is not (nothing in build/container/compose.yml
// bind-mounts it). writeContainerSpec must materialize the sandbox spec/
// runner-state pair under ContainerBackendOptions.RuntimeDir (the same
// bind-mounted-source==target BOID_RUNTIME_DIR every other DooD-visible
// artifact, e.g. dockerTLSCertDir, already uses) instead of /tmp whenever
// RuntimeDir is configured.

// TestContainerBackend_Launch_SpecState_UnderRuntimeDir_WhenConfigured pins
// the fix: with RuntimeDir set, the bind-mounted spec/state file Sources
// must live under <RuntimeDir>/spec/<spec.ID>/, not /tmp.
func TestContainerBackend_Launch_SpecState_UnderRuntimeDir_WhenConfigured(t *testing.T) {
	runtimeDir := t.TempDir()
	waitBlock := make(chan struct{})
	api := &fakeDockerAPI{
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			<-waitBlock
			resCh := make(chan container.WaitResponse, 1)
			resCh <- container.WaitResponse{StatusCode: 0}
			return client.ContainerWaitResult{Result: resCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir})

	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-spec-dir", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-spec-dir"})
	defer close(waitBlock)

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	mounts := api.createCalls[0].HostConfig.Mounts

	var specSource, stateSource string
	for _, m := range mounts {
		switch m.Target {
		case containerSpecPath:
			specSource = m.Source
		case containerStatePath:
			stateSource = m.Source
		}
	}
	if specSource == "" || stateSource == "" {
		t.Fatalf("spec/state bind mounts missing: %+v", mounts)
	}

	wantDir := filepath.Join(runtimeDir, "spec", "job-spec-dir")
	if !strings.HasPrefix(specSource, wantDir) {
		t.Errorf("spec mount Source = %q, want it under %q (RuntimeDir, not /tmp)", specSource, wantDir)
	}
	if !strings.HasPrefix(stateSource, wantDir) {
		t.Errorf("state mount Source = %q, want it under %q (RuntimeDir, not /tmp)", stateSource, wantDir)
	}
	// Blocker 1's actual contract is "under RuntimeDir", not "never under
	// /tmp" — t.TempDir() itself can legitimately resolve under /tmp on some
	// systems. The flat, unscoped `/tmp/boid-<ID>-runner-spec.json` literal
	// (the pre-fix layout) is what must never appear once RuntimeDir is set;
	// that is what the wantDir prefix check above already pins.
	if specSource == "/tmp/boid-job-spec-dir-runner-spec.json" {
		t.Errorf("spec mount Source = %q, want the RuntimeDir-scoped path, not the pre-fix flat /tmp layout", specSource)
	}

	if _, err := os.Stat(specSource); err != nil {
		t.Errorf("spec file not materialized at %q: %v", specSource, err)
	}

	_ = sess.ID()
}

// TestContainerBackend_Launch_SpecState_FallsBackToTmp_WhenRuntimeDirUnset
// pins the pre-PR7 behavior for every caller that hasn't opted into
// RuntimeDir (every existing unit test, and any deploy that hasn't wired
// it): the flat /tmp/boid-<ID>-runner-{spec,state}.json layout stays
// byte-for-byte unchanged.
func TestContainerBackend_Launch_SpecState_FallsBackToTmp_WhenRuntimeDirUnset(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	mustLaunch(t, be, sandbox.Spec{ID: "job-spec-tmp", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-spec-tmp"})

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	mounts := api.createCalls[0].HostConfig.Mounts
	var specSource string
	for _, m := range mounts {
		if m.Target == containerSpecPath {
			specSource = m.Source
		}
	}
	want := "/tmp/boid-job-spec-tmp-runner-spec.json"
	if specSource != want {
		t.Errorf("spec mount Source = %q, want %q (unchanged /tmp layout when RuntimeDir is unset)", specSource, want)
	}
}

// TestContainerBackend_Launch_SpecDir_RemovedOnExit pins that the per-job
// <runtimeDir>/spec/<spec.ID>/ directory this backend creates when
// RuntimeDir is configured does not leak once the container exits.
func TestContainerBackend_Launch_SpecDir_RemovedOnExit(t *testing.T) {
	runtimeDir := t.TempDir()
	waitCh := make(chan container.WaitResponse, 1)
	waitCh <- container.WaitResponse{StatusCode: 0}
	api := &fakeDockerAPI{
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: waitCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-spec-cleanup", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-spec-cleanup"})

	if _, err := sess.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	specDir := filepath.Join(runtimeDir, "spec", "job-spec-cleanup")
	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(specDir); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("spec dir %q still exists after container exit, want it removed", specDir)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
