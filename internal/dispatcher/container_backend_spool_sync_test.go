package dispatcher

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// TestContainerSession_WaitLoop_SyncsTranscriptSpoolBeforeClose pins [Major
// 9, PR7 codex review]: the transcript spool file must be fsync'd before
// Close, not just Close'd — a Close alone only flushes userspace buffers to
// the kernel, not the kernel's own buffers to stable storage, so a crash
// between Close and the data actually landing on disk could still lose the
// transcript tail exactly when a container is about to be removed. This
// test can't observe the fsync syscall directly (no fake filesystem layer
// here), so it pins the observable contract instead: content written
// before exit must be fully readable via ReadTranscript once Wait returns,
// regardless of timing — the same assertion TestContainerSession_
// TranscriptSpool_SurvivesContainerRemove already makes, kept here as this
// fix's own regression guard so a future refactor that reorders Sync/Close
// relative to Wait's return is caught here specifically.
func TestContainerSession_WaitLoop_SyncsTranscriptSpoolBeforeClose(t *testing.T) {
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
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-sync", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-sync"})

	want := "synced before close"
	conn.feedFrame(1, []byte(want))
	waitCh <- container.WaitResponse{StatusCode: 0}
	conn.Close()

	if _, err := sess.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// waitLoop's Sync+Close both run synchronously before s.done closes
	// (see its own doc comment), so no polling is needed — Wait returning
	// is itself the synchronization point, same as
	// TestContainerSession_TranscriptSpool_SurvivesContainerRemove.
	data, err := ReadTranscript(runtimeDir, sess.ID())
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if string(data) != want {
		t.Errorf("transcript = %q, want %q", data, want)
	}
}
