package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins [Major 8, PR7 codex review]
// (docs/plans/phase6-container-backend.md §決定8's transcript full-
// persistence contract): a disk-spool failure must fail Launch hard rather
// than silently degrade to an in-memory-only transcript that disappears the
// moment the container is removed.

// TestContainerBackend_Launch_TranscriptSpoolOpenFailure_FailsHard pins
// [Major 8]: when RuntimeDir is configured but the spool directory cannot
// be created (e.g. a stale FILE occupies the path a directory is needed at
// — standing in for "runtimes filesystem full/unwritable"), Launch must
// fail hard: no container is left running, and the caller gets a non-nil
// error instead of a job that silently loses `boid job log` the moment its
// container is removed.
func TestContainerBackend_Launch_TranscriptSpoolOpenFailure_FailsHard(t *testing.T) {
	runtimeDir := t.TempDir()
	const containerID = "fake-container-1"
	// Occupy the directory openTranscriptSpool needs (<runtimeDir>/<containerID>)
	// with a plain file, so its os.MkdirAll call fails with ENOTDIR/EEXIST.
	if err := os.WriteFile(filepath.Join(runtimeDir, containerID), []byte("occupied"), 0o644); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}

	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir})

	_, err := be.Launch(context.Background(), sandbox.Spec{ID: "job-spool-open-fail", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-spool-open-fail"})
	if err == nil {
		t.Fatal("Launch() = nil error, want a hard failure when the transcript spool cannot be opened")
	}

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1 (create happens before the spool open)", len(api.createCalls))
	}
	if len(api.removeIDs) != 1 || api.removeIDs[0] != containerID {
		t.Errorf("ContainerRemove calls = %v, want the just-created container %q torn down", api.removeIDs, containerID)
	}
	if len(api.attachIDs) != 0 {
		t.Errorf("ContainerAttach calls = %v, want none (Launch must fail before ever attaching)", api.attachIDs)
	}
	if len(api.startIDs) != 0 {
		t.Errorf("ContainerStart calls = %v, want none (Launch must fail before starting)", api.startIDs)
	}
}

// TestContainerBackend_Launch_TranscriptSpoolDisabled_StillSucceeds pins
// the companion non-regression: RuntimeDir unset (spooling intentionally
// disabled, not a failure) must never fail Launch — matching every
// pre-Major-8 test's expectation.
func TestContainerBackend_Launch_TranscriptSpoolDisabled_StillSucceeds(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	sess, err := be.Launch(context.Background(), sandbox.Spec{ID: "job-spool-disabled", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-spool-disabled"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if sess == nil {
		t.Fatal("Launch returned a nil session with a nil error")
	}
}
