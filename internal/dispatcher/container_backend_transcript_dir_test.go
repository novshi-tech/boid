package dispatcher

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins ContainerBackendOptions.TranscriptDir — the persistence fix
// for "every job transcript is lost on host reboot" (BOID_RUNTIME_DIR is
// host tmpfs; see hostVisibleRuntimesDirFor's own doc comment in
// internal/server/wire.go). transcript.log/diagnostics.json are written
// exclusively by the daemon process itself (never bind-mounted into a job
// container the way spec.json/state.json are), so unlike RuntimeDir they
// don't need host-visibility and can be redirected to a persistent volume.

// TestContainerBackend_TranscriptDir_OverridesRuntimeDir pins that when both
// RuntimeDir and TranscriptDir are set, openTranscriptSpool spools under
// TranscriptDir, not RuntimeDir — the persistent root wins.
func TestContainerBackend_TranscriptDir_OverridesRuntimeDir(t *testing.T) {
	runtimeDir := t.TempDir()
	transcriptDir := t.TempDir()
	conn := newFakeAttachConn()
	waitCh := make(chan container.WaitResponse, 1)

	api := &fakeDockerAPI{
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(conn, "")}, nil
		},
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: waitCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir, TranscriptDir: transcriptDir})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-transcript-dir", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-transcript-dir"})

	want := "spooled to transcript dir"
	conn.feedFrame(1, []byte(want))
	waitCh <- container.WaitResponse{StatusCode: 0}
	conn.Close()

	if _, err := sess.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	data, err := ReadTranscript(transcriptDir, sess.ID())
	if err != nil {
		t.Fatalf("ReadTranscript(transcriptDir): %v", err)
	}
	if string(data) != want {
		t.Errorf("transcript under transcriptDir = %q, want %q", data, want)
	}

	if _, err := ReadTranscript(runtimeDir, sess.ID()); err == nil {
		t.Error("ReadTranscript(runtimeDir) succeeded, want it to be empty (TranscriptDir must win, not append to both)")
	}
}

// TestContainerBackend_TranscriptDir_Unset_FallsBackToRuntimeDir pins the
// non-regression: TranscriptDir empty (every pre-this-field caller) must
// spool under RuntimeDir exactly as before this field existed.
func TestContainerBackend_TranscriptDir_Unset_FallsBackToRuntimeDir(t *testing.T) {
	runtimeDir := t.TempDir()
	conn := newFakeAttachConn()
	waitCh := make(chan container.WaitResponse, 1)

	api := &fakeDockerAPI{
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(conn, "")}, nil
		},
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: waitCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-no-transcript-dir", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-no-transcript-dir"})

	want := "spooled to runtime dir"
	conn.feedFrame(1, []byte(want))
	waitCh <- container.WaitResponse{StatusCode: 0}
	conn.Close()

	if _, err := sess.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	data, err := ReadTranscript(runtimeDir, sess.ID())
	if err != nil {
		t.Fatalf("ReadTranscript(runtimeDir): %v", err)
	}
	if string(data) != want {
		t.Errorf("transcript under runtimeDir = %q, want %q", data, want)
	}
}
