package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins the containerBackend/containerSession contract (docs/plans/
// phase6-container-backend.md §PR5) against the fake dockerAPI in
// container_backend_fake_test.go. containerBackend is not wired into
// production dispatch as of PR5 — see NewContainerBackend's doc comment —
// so every test here drives it directly rather than through Runner.

func mustLaunch(t *testing.T, be backend.SandboxBackend, spec sandbox.Spec, opts backend.LaunchOptions) backend.SandboxSession {
	t.Helper()
	sess, err := be.Launch(context.Background(), spec, opts)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return sess
}

func TestContainerBackend_Launch_CreatesContainerWithHostConfigInit(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1"})
	if sess.ID() == "" {
		t.Fatal("Launch returned a session with an empty ID")
	}

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	hostCfg := api.createCalls[0].HostConfig
	if hostCfg == nil || hostCfg.Init == nil || !*hostCfg.Init {
		t.Fatalf("HostConfig.Init = %+v, want a non-nil pointer to true (§決定 3: docker-init as PID 1)", hostCfg)
	}

	if len(api.startIDs) != 1 || api.startIDs[0] != sess.ID() {
		t.Fatalf("ContainerStart calls = %v, want exactly [%s]", api.startIDs, sess.ID())
	}
}

func TestContainerBackend_Launch_MountSourceKindMapping(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	spec := sandbox.Spec{
		ID:   "job-mounts",
		Argv: []string{"true"},
		Mounts: []sandbox.Mount{
			{
				// HostPath: an absolute host Source not targeting /workspace.
				Source: "/home/boid-user/.local/share/boid/homes/default",
				Target: "/home/boid",
				Type:   sandbox.MountBind,
			},
			{
				// ContainerLocal: targets the sandbox-internal clone dir
				// (isWorkspaceLocalTarget), regardless of Source (§決定 4).
				Source: "/host/runtime/dir/workspace",
				Target: "/workspace/myproject",
				Type:   sandbox.MountBind,
			},
			{
				// NamedVolume: a non-absolute, non-empty Source.
				Source: "boid-named-volume",
				Target: "/mnt/named",
				Type:   sandbox.MountBind,
			},
		},
	}
	mustLaunch(t, be, spec, backend.LaunchOptions{JobID: "job-mounts"})

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	mounts := api.createCalls[0].HostConfig.Mounts

	find := func(target string) (mount.Mount, bool) {
		for _, m := range mounts {
			if m.Target == target {
				return m, true
			}
		}
		return mount.Mount{}, false
	}

	homeMount, ok := find("/home/boid")
	if !ok {
		t.Fatal("no docker Mount entry for /home/boid (expected HostPath bind)")
	}
	if homeMount.Type != mount.TypeBind || homeMount.Source != "/home/boid-user/.local/share/boid/homes/default" {
		t.Errorf("/home/boid mount = %+v, want Type=bind Source=/home/boid-user/.local/share/boid/homes/default", homeMount)
	}

	if _, ok := find("/workspace/myproject"); ok {
		t.Error("/workspace/myproject got a docker Mount entry, want none (MountSourceContainerLocal has no host-side counterpart, §決定 4)")
	}

	volMount, ok := find("/mnt/named")
	if !ok {
		t.Fatal("no docker Mount entry for /mnt/named (expected NamedVolume mount)")
	}
	if volMount.Type != mount.TypeVolume || volMount.Source != "boid-named-volume" {
		t.Errorf("/mnt/named mount = %+v, want Type=volume Source=boid-named-volume", volMount)
	}

	// The spec/state files are always bind-mounted at the fixed
	// sandbox-internal paths, in addition to the spec.Mounts translation
	// above.
	if _, ok := find(containerSpecPath); !ok {
		t.Errorf("no docker Mount entry for %s (spec file bind)", containerSpecPath)
	}
	if _, ok := find(containerStatePath); !ok {
		t.Errorf("no docker Mount entry for %s (state file bind)", containerStatePath)
	}
}

// TestContainerBackend_Launch_WorkingDirNeverTargetsUncreatedCloneSubdir
// pins the PR9 e2e-container CI fix: docker's own `--workdir` (Config.
// WorkingDir) must never be set to the per-project clone TARGET
// subdirectory itself ("/workspace/<name>") — only the always-present,
// always-correctly-owned parent ("/workspace", sandboxCloneTargetDir) is
// baked into the image at build time (build/container/Dockerfile). A
// not-yet-existing WorkingDir is auto-created by docker as root before the
// entrypoint process's --user takes effect (the same gotcha the Dockerfile
// itself documents for its own build-time WORKDIR instruction), which left
// the per-project leaf owned by root and unwritable by the job uid — the
// real-docker e2e-container CI job's "git clone ... /.git: Permission
// denied" failure.
func TestContainerBackend_Launch_WorkingDirNeverTargetsUncreatedCloneSubdir(t *testing.T) {
	tests := []struct {
		name    string
		workDir string
		want    string
	}{
		{
			name:    "clone target subdir rewritten to workspace parent",
			workDir: "/workspace/myproject",
			want:    "/workspace",
		},
		{
			name:    "bare workspace root left unchanged",
			workDir: "/workspace",
			want:    "/workspace",
		},
		{
			name:    "non-workspace workdir (e.g. home dir) left unchanged",
			workDir: "/home/boid",
			want:    "/home/boid",
		},
		{
			name:    "empty workdir left unchanged",
			workDir: "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeDockerAPI{}
			be := NewContainerBackend(api, ContainerBackendOptions{})

			mustLaunch(t, be, sandbox.Spec{ID: "job-workdir", Argv: []string{"true"}, WorkDir: tt.workDir}, backend.LaunchOptions{JobID: "job-workdir"})

			if len(api.createCalls) != 1 {
				t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
			}
			if got := api.createCalls[0].Config.WorkingDir; got != tt.want {
				t.Errorf("Config.WorkingDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContainerBackend_Launch_UserFlagAndPasswdEntry(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{UID: intPtr(1000), GID: intPtr(1000)})

	mustLaunch(t, be, sandbox.Spec{ID: "job-uid", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-uid"})

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	// §決定 4: job containers run --user <uid>:<gid> (non-root), matching
	// the /etc/passwd entry build/container/Dockerfile bakes for that same
	// uid/gid at image-build time (verified by the image build, not this
	// unit test — this test pins the --user flag format only).
	if got, want := api.createCalls[0].Config.User, "1000:1000"; got != want {
		t.Errorf("Config.User = %q, want %q", got, want)
	}
}

// TestContainerBackend_Launch_RejectsPartialOrRootUIDGID pins Major 1 from
// the PR5 review: a partial uid/gid override (only one of the two set) or
// one that resolves to root (either side == 0, e.g. `UID: 0, GID: 1000`)
// must not reach `--user`; both must fall back to the non-root default
// (§決定 4). Only a fully-specified, fully-non-zero pair is honored as-is.
func TestContainerBackend_Launch_RejectsPartialOrRootUIDGID(t *testing.T) {
	tests := []struct {
		name     string
		uid, gid *int
		wantUser string
	}{
		{name: "both unset falls back to default", uid: nil, gid: nil, wantUser: "1000:1000"},
		{name: "uid=0 with nonzero gid falls back to default", uid: intPtr(0), gid: intPtr(1000), wantUser: "1000:1000"},
		{name: "gid=0 with nonzero uid falls back to default", uid: intPtr(1000), gid: intPtr(0), wantUser: "1000:1000"},
		{name: "uid=0 gid=0 falls back to default", uid: intPtr(0), gid: intPtr(0), wantUser: "1000:1000"},
		{name: "only uid set falls back to default", uid: intPtr(2000), gid: nil, wantUser: "1000:1000"},
		{name: "only gid set falls back to default", uid: nil, gid: intPtr(2000), wantUser: "1000:1000"},
		{name: "fully specified nonzero pair is honored", uid: intPtr(2000), gid: intPtr(2001), wantUser: "2000:2001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeDockerAPI{}
			be := NewContainerBackend(api, ContainerBackendOptions{UID: tt.uid, GID: tt.gid})
			mustLaunch(t, be, sandbox.Spec{ID: "job-uidgid", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-uidgid"})
			if len(api.createCalls) != 1 {
				t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
			}
			if got := api.createCalls[0].Config.User; got != tt.wantUser {
				t.Errorf("Config.User = %q, want %q", got, tt.wantUser)
			}
		})
	}
}

// intPtr returns a pointer to v, for constructing ContainerBackendOptions.UID/GID
// (nullable *int fields — see their doc comment) in test literals.
func intPtr(v int) *int { return &v }

// TestContainerBackend_Launch_CreatesNamedVolumesWithReapLabels pins Major
// 6 from the PR5 review: a MountSourceNamedVolume mount must be explicitly
// VolumeCreate'd (carrying boid.job_id / boid.install_id) before
// ContainerCreate implicitly references it — otherwise Docker
// auto-creates it unlabeled and ReapOrphans's volume sweep can never find
// it.
func TestContainerBackend_Launch_CreatesNamedVolumesWithReapLabels(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	spec := sandbox.Spec{
		ID:   "job-named-vol",
		Argv: []string{"true"},
		Mounts: []sandbox.Mount{
			{Source: "boid-home-workspace-foo", Target: "/mnt/named", Type: sandbox.MountBind},
		},
	}
	mustLaunch(t, be, spec, backend.LaunchOptions{JobID: "job-named-vol"})

	if len(api.volumeCreateCalls) != 1 {
		t.Fatalf("VolumeCreate calls = %d, want 1", len(api.volumeCreateCalls))
	}
	call := api.volumeCreateCalls[0]
	if call.Name != "boid-home-workspace-foo" {
		t.Errorf("VolumeCreate Name = %q, want %q", call.Name, "boid-home-workspace-foo")
	}
	if got, want := call.Labels[labelJobID], "job-named-vol"; got != want {
		t.Errorf("VolumeCreate Labels[%q] = %q, want %q", labelJobID, got, want)
	}
	if got, want := call.Labels[labelInstallID], "install-xyz"; got != want {
		t.Errorf("VolumeCreate Labels[%q] = %q, want %q", labelInstallID, got, want)
	}

	// VolumeCreate must happen before ContainerCreate references the
	// volume by name.
	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
}

// TestContainerBackend_Launch_LabelsWorkspaceUnconditionally pins the PR5
// review's Minor finding: boid.workspace must be set from
// opts.Workspace unconditionally — present (even as an empty string) when
// Workspace is unset, not omitted from Labels entirely.
func TestContainerBackend_Launch_LabelsWorkspaceUnconditionally(t *testing.T) {
	t.Run("workspace set", func(t *testing.T) {
		api := &fakeDockerAPI{}
		be := NewContainerBackend(api, ContainerBackendOptions{})
		mustLaunch(t, be, sandbox.Spec{ID: "job-ws-set", Argv: []string{"true"}},
			backend.LaunchOptions{JobID: "job-ws-set", Workspace: "myworkspace"})

		if len(api.createCalls) != 1 {
			t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
		}
		if got, want := api.createCalls[0].Config.Labels[labelWorkspace], "myworkspace"; got != want {
			t.Errorf("Labels[%q] = %q, want %q", labelWorkspace, got, want)
		}
	})

	t.Run("workspace empty still gets an explicit empty-string label", func(t *testing.T) {
		api := &fakeDockerAPI{}
		be := NewContainerBackend(api, ContainerBackendOptions{})
		mustLaunch(t, be, sandbox.Spec{ID: "job-ws-empty", Argv: []string{"true"}},
			backend.LaunchOptions{JobID: "job-ws-empty"})

		if len(api.createCalls) != 1 {
			t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
		}
		labels := api.createCalls[0].Config.Labels
		got, ok := labels[labelWorkspace]
		if !ok {
			t.Fatal("Labels missing boid.workspace key entirely, want it present (even if empty)")
		}
		if got != "" {
			t.Errorf("Labels[%q] = %q, want empty string", labelWorkspace, got)
		}
	})
}

func TestContainerBackend_Adopt_ReconstructsSessionFromRunningContainer(t *testing.T) {
	const runtimeID = "already-running-container"
	tty := false

	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					State:  &container.State{Running: true},
					Config: &container.Config{Tty: tty},
				},
			}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	sess, ok := be.Adopt(context.Background(), runtimeID)
	if !ok {
		t.Fatal("Adopt returned ok=false for a running container")
	}
	if sess.ID() != runtimeID {
		t.Errorf("Adopt session ID = %q, want %q", sess.ID(), runtimeID)
	}
	if len(api.inspectIDs) != 1 || api.inspectIDs[0] != runtimeID {
		t.Errorf("ContainerInspect calls = %v, want exactly [%s]", api.inspectIDs, runtimeID)
	}
	if len(api.attachIDs) != 1 || api.attachIDs[0] != runtimeID {
		t.Errorf("ContainerAttach calls = %v, want exactly [%s]", api.attachIDs, runtimeID)
	}
	if !api.attachCalls[0].Logs {
		t.Error("Adopt's ContainerAttach did not request Logs:true (post-restart output replay)")
	}

	// A second Adopt for the same runtimeID must reuse the cached session
	// (see containerBackend.Adopt's doc comment) rather than attach again.
	sess2, ok := be.Adopt(context.Background(), runtimeID)
	if !ok || sess2 != sess {
		t.Errorf("second Adopt(%q) = (%v, %v), want the same cached session", runtimeID, sess2, ok)
	}
	if len(api.attachIDs) != 1 {
		t.Errorf("ContainerAttach calls after second Adopt = %d, want still 1 (session cache hit)", len(api.attachIDs))
	}
}

func TestContainerBackend_Adopt_NotRunningReportsNotOK(t *testing.T) {
	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{Running: false}}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	if _, ok := be.Adopt(context.Background(), "exited-container"); ok {
		t.Error("Adopt returned ok=true for a non-running container")
	}
}

// TestContainerBackend_Adopt_DoesNotOpenFreshTranscriptSpool pins
// openTranscriptSpool's documented scope boundary (§決定8, PR7): even with
// RuntimeDir configured, an Adopt-reconstructed session must NOT open a
// fresh (truncating) spool file — doing so would either destroy
// pre-restart transcript.log content (O_TRUNC) or duplicate it (O_APPEND,
// since Adopt's own `Logs: true` attach replays the container's full
// history through appendTranscript again). RuntimeExit.TranscriptPath
// staying empty is the externally observable signal that no fresh spool
// was opened.
func TestContainerBackend_Adopt_DoesNotOpenFreshTranscriptSpool(t *testing.T) {
	const runtimeID = "adopted-container-no-fresh-spool"
	runtimeDir := t.TempDir()
	waitCh := make(chan container.WaitResponse, 1)

	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					State:  &container.State{Running: true},
					Config: &container.Config{Tty: false},
				},
			}, nil
		},
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: waitCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{RuntimeDir: runtimeDir})

	sess, ok := be.Adopt(context.Background(), runtimeID)
	if !ok {
		t.Fatal("Adopt returned ok=false")
	}

	waitCh <- container.WaitResponse{StatusCode: 0}
	exit, err := sess.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.TranscriptPath != "" {
		t.Errorf("TranscriptPath = %q, want empty (Adopt-reconstructed sessions do not open a fresh spool)", exit.TranscriptPath)
	}
	if _, statErr := os.Stat(filepath.Join(runtimeDir, runtimeID)); !os.IsNotExist(statErr) {
		t.Errorf("expected no runtime dir to be created for an adopted session, stat err = %v", statErr)
	}
}

// TestContainerBackend_Adopt_ConcurrentCacheMissSharesOneAttach pins Major
// 5 from the PR5 review: two concurrent Adopt calls for the same
// (not-yet-cached) runtimeID must not each start their own
// inspect/attach — that would create two independent ContainerWait owners
// for the same container, breaking §決定 7's single-owner contract. Only
// one attach may happen; both callers must observe the same session.
func TestContainerBackend_Adopt_ConcurrentCacheMissSharesOneAttach(t *testing.T) {
	const runtimeID = "concurrent-adopt-container"
	inspectStarted := make(chan struct{})
	releaseInspect := make(chan struct{})
	var attachCount int32

	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			close(inspectStarted)
			<-releaseInspect
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					State:  &container.State{Running: true},
					Config: &container.Config{},
				},
			}, nil
		},
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			atomic.AddInt32(&attachCount, 1)
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(newFakeAttachConn(), "")}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	var wg sync.WaitGroup
	sessions := make([]backend.SandboxSession, 2)
	oks := make([]bool, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessions[i], oks[i] = be.Adopt(context.Background(), runtimeID)
		}(i)
	}

	<-inspectStarted
	// Give the second Adopt call time to reach and block on the in-flight
	// reservation, rather than trivially winning a race where it would
	// start its own inspect before the first even gets here.
	time.Sleep(20 * time.Millisecond)
	close(releaseInspect)
	wg.Wait()

	if !oks[0] || !oks[1] {
		t.Fatalf("Adopt ok = (%v, %v), want (true, true)", oks[0], oks[1])
	}
	if sessions[0] != sessions[1] {
		t.Errorf("concurrent Adopt calls returned different sessions, want the same shared session")
	}
	if got := atomic.LoadInt32(&attachCount); got != 1 {
		t.Errorf("ContainerAttach was called %d times, want exactly 1 (Major 5: single adoption owner)", got)
	}
	if got := len(api.inspectIDs); got != 1 {
		t.Errorf("ContainerInspect was called %d times, want exactly 1", got)
	}
}

// TestContainerSession_Subscribe_ReportsNotOkWhenAdoptAttachFailed pins the
// first half of the fix for the bug Opus's review of PR #857 found:
// doAdopt's own best-effort attach (its doc comment) can fail against a
// genuinely running container (the engine was merely unreachable at
// adoption time — here, ContainerAttach always returns
// context.DeadlineExceeded), and pre-fix that left a session with
// running=true but no live stream at all. Subscribe() checked only
// running, so it answered ok=true with a channel that would then never
// receive a single byte — the "connected but the terminal is silent
// forever" symptom. Requiring attached too (see Subscribe's own doc
// comment) means this case now honestly reports ok=false instead.
func TestContainerSession_Subscribe_ReportsNotOkWhenAdoptAttachFailed(t *testing.T) {
	const runtimeID = "runtime-slow-attach"
	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					State:  &container.State{Running: true},
					Config: &container.Config{},
				},
			}, nil
		},
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			return client.ContainerAttachResult{}, context.DeadlineExceeded
		},
		// Block forever: the container is genuinely still running (the
		// engine answered ContainerInspect fine) — only the attach half is
		// broken. Without this override the default ContainerWait resolves
		// immediately and races this test's own Subscribe call against
		// waitLoop's teardown.
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: make(chan container.WaitResponse), Error: make(chan error)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	sess, ok := be.Adopt(context.Background(), runtimeID)
	if !ok {
		t.Fatal("Adopt returned ok=false; want ok=true (a failed attach alone must not fail Adopt itself — signal/stop/wait must still work)")
	}

	_, ch, _, subOK, finished := sess.Subscribe()
	if subOK {
		t.Error("Subscribe returned ok=true for a session whose only attach attempt failed; want ok=false (no live stream to subscribe to)")
	}
	if ch != nil {
		t.Error("Subscribe returned a non-nil channel alongside ok=false")
	}
	// finished must be false here (Opus review of PR #857, B2): the
	// container is genuinely still running (ContainerWaitFunc above never
	// resolves) — only the attach half failed. A caller (ws_attach.go/
	// job_log_sse.go) that read ok=false as "job finished" regardless of
	// this value would tell an operator a still-running job had exited
	// successfully, which is the false positive B2 found.
	if finished {
		t.Error("Subscribe reported finished=true for a session whose container is still genuinely running; want false")
	}
}

// TestContainerBackend_Adopt_CacheHitReattachesAfterEngineRecovers pins the
// second half of the fix (Opus review of PR #857): a cache-HIT Adopt call
// for a running-but-not-attached session (the shape
// TestContainerSession_Subscribe_ReportsNotOkWhenAdoptAttachFailed just
// above pins the symptom of) must best-effort re-attach, so that once the
// engine recovers the very next Adopt/Subscribe call gets a live stream
// again — without needing a daemon restart, which was the ONLY recovery
// path before this fix (doAdopt only ever runs once per runtimeID; nothing
// ever retried a failed attach on an already-cached session).
func TestContainerBackend_Adopt_CacheHitReattachesAfterEngineRecovers(t *testing.T) {
	const runtimeID = "runtime-slow-attach"
	var attachAttempts int32
	var connMu sync.Mutex
	var conn *fakeAttachConn
	var attachLogsFlags []bool

	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					State:  &container.State{Running: true},
					Config: &container.Config{Tty: false},
				},
			}, nil
		},
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			connMu.Lock()
			attachLogsFlags = append(attachLogsFlags, options.Logs)
			connMu.Unlock()
			if atomic.AddInt32(&attachAttempts, 1) == 1 {
				// The first attach attempt (doAdopt's own) simulates the
				// engine being unreachable, matching the reviewer's
				// real-world repro ("context deadline exceeded" against a
				// fake engine whose inspect succeeds but attach hangs).
				return client.ContainerAttachResult{}, context.DeadlineExceeded
			}
			// Every later attempt (the engine has "recovered") succeeds.
			connMu.Lock()
			conn = newFakeAttachConn()
			c := conn
			connMu.Unlock()
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(c, "")}, nil
		},
		// Block forever: the container stays genuinely running across both
		// Adopt calls in this test — only the attach half recovers.
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: make(chan container.WaitResponse), Error: make(chan error)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	sess, ok := be.Adopt(context.Background(), runtimeID)
	if !ok {
		t.Fatal("first Adopt returned ok=false")
	}
	if _, _, _, subOK, _ := sess.Subscribe(); subOK {
		t.Fatal("Subscribe returned ok=true right after a failed adopt attach; want ok=false")
	}
	if got := atomic.LoadInt32(&attachAttempts); got != 1 {
		t.Fatalf("ContainerAttach calls after first Adopt = %d, want 1", got)
	}

	// "Engine recovers": a second Adopt for the same runtimeID is a cache
	// hit (same session, same ID), and must trigger a best-effort
	// re-attach rather than silently reusing the dead one.
	sess2, ok := be.Adopt(context.Background(), runtimeID)
	if !ok || sess2 != sess {
		t.Fatalf("second Adopt = (%v, %v), want the same cached session with ok=true", sess2, ok)
	}
	if got := atomic.LoadInt32(&attachAttempts); got != 2 {
		t.Fatalf("ContainerAttach calls after second Adopt = %d, want 2 (cache-hit re-attach actually fired)", got)
	}
	// This session's transcript is still empty at this point (the FIRST
	// attach never got far enough to produce anything) — replaying the
	// container's pre-existing logs is not just safe here, it's the only
	// way to backfill whatever ran before this daemon ever knew about the
	// container. See reattachIfLost's own doc comment (Opus review of PR
	// #857, B1) for why this must NOT also be true for a re-attach whose
	// transcript already has content (TestContainerBackend_Adopt_ReattachAfterMidStreamDropDoesNotDuplicateTranscript
	// pins that side).
	connMu.Lock()
	gotLogsFlags := append([]bool(nil), attachLogsFlags...)
	connMu.Unlock()
	if len(gotLogsFlags) != 2 || !gotLogsFlags[1] {
		t.Fatalf("attach Logs flags = %v, want [_, true] (empty-transcript reattach must still replay pre-existing container output)", gotLogsFlags)
	}

	snapshot, ch, cancel, subOK, _ := sess.Subscribe()
	if !subOK {
		t.Fatal("Subscribe returned ok=false after a successful re-attach, want ok=true")
	}
	defer cancel()
	_ = snapshot

	connMu.Lock()
	c := conn
	connMu.Unlock()
	c.feedFrame(1, []byte("hello after recovery"))

	want := "hello after recovery"
	if got := readChunkTimeout(t, ch); string(got) != want {
		t.Errorf("subscriber chunk after re-attach = %q, want %q", got, want)
	}

	// Ending the RE-ATTACHED generation's stream must not panic. This is
	// the implementation trap the fix's own doc comments warn about
	// (containerSession.readDone / attach's doc comments): a first-draft
	// fix that re-attaches but keeps a single, session-lifetime readDone
	// channel would have this second generation's readLoop defer
	// `close()` a channel the FIRST (failed) attach's error path already
	// closed — close of an already-closed channel panics, and since
	// readLoop runs in its own goroutine, that panic crashes the whole
	// test binary rather than merely failing this one test. Driving the
	// second generation's connection closed here and waiting for
	// Subscribe to observe attached go back to false is what forces that
	// deferred close to actually run inside this test's own lifetime,
	// rather than asynchronously after it (or some later test) has
	// already reported PASS.
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, _, subOK, _ := sess.Subscribe(); !subOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the re-attached stream to end after closing its connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A little extra headroom past readLoop's own attached=false
	// assignment for its very next statement (close(readDone)) to
	// actually execute — see readLoop's own doc comment for why these two
	// are sequenced. There is no assertion to add beyond letting this
	// statement run and the process still be alive afterward: a
	// double-close panic here would abort the whole `go test` run, not
	// just this test.
	time.Sleep(50 * time.Millisecond)
}

// TestContainerBackend_Adopt_ConcurrentCacheHitReattachIsRaceSafe pins that
// reattachIfLost is safe under concurrent invocation (multiple WS ingress
// racing a cache hit for the same session right as the engine recovers —
// the concurrency concern flagged in the review alongside the two behavior
// fixes above). Many concurrent Adopt calls against one running-but-not-
// attached session must not each fire their own ContainerAttach (which
// would, among other things, leak attach connections) and must not
// panic/race (run under `go test -race`, per this repo's own quality bar
// for this file).
func TestContainerBackend_Adopt_ConcurrentCacheHitReattachIsRaceSafe(t *testing.T) {
	const runtimeID = "concurrent-reattach-container"
	const n = 8
	var attachAttempts int32
	// concurrentAttachers counts how many ContainerAttach calls are
	// SIMULTANEOUSLY blocked inside the "re-attach" branch below, waiting
	// on releaseAttach — the deterministic rendezvous a broken dedup would
	// be caught by (Opus review of PR #857, N5). A naive "just launch n
	// goroutines and count attachAttempts at the end" version of this test
	// (this file's own first draft) only caught a missing-dedup mutation
	// 19/30 runs: without something forcing concurrent callers to actually
	// overlap, whichever goroutine's Adopt call happens to run first can
	// finish its own attach and flip s.attached=true before a second
	// goroutine ever reaches the check, in which case even BROKEN dedup
	// code never gets a chance to double-fire — the test's own scheduling
	// luck was doing the work, not the assertion. Blocking every re-attach
	// call here until the test explicitly releases it removes that
	// dependency on luck: every one of the n goroutines below is
	// guaranteed to have reached (or been correctly turned away from) this
	// point before the test inspects concurrentAttachers.
	var concurrentAttachers int32
	var maxConcurrentAttachers int32
	releaseAttach := make(chan struct{})

	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					State:  &container.State{Running: true},
					Config: &container.Config{},
				},
			}, nil
		},
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			if atomic.AddInt32(&attachAttempts, 1) == 1 {
				// The initial doAdopt-equivalent attach: fails, populating
				// the cache with a running-but-not-attached session — the
				// state every one of the n concurrent Adopt calls below
				// will find.
				return client.ContainerAttachResult{}, errors.New("engine unreachable")
			}
			cur := atomic.AddInt32(&concurrentAttachers, 1)
			for {
				prevMax := atomic.LoadInt32(&maxConcurrentAttachers)
				if cur <= prevMax || atomic.CompareAndSwapInt32(&maxConcurrentAttachers, prevMax, cur) {
					break
				}
			}
			<-releaseAttach
			atomic.AddInt32(&concurrentAttachers, -1)
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(newFakeAttachConn(), "")}, nil
		},
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: make(chan container.WaitResponse), Error: make(chan error)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	// Populate the cache with a not-attached-but-running session first —
	// exactly the state the "engine was down at adoption time" scenario
	// leaves behind (TestContainerSession_Subscribe_ReportsNotOkWhenAdoptAttachFailed
	// pins this half on its own).
	sess, ok := be.Adopt(context.Background(), runtimeID)
	if !ok {
		t.Fatal("initial Adopt returned ok=false")
	}
	if _, _, _, subOK, _ := sess.Subscribe(); subOK {
		t.Fatal("Subscribe ok=true before any successful attach")
	}

	// Now fire many concurrent cache-hit Adopt calls, as several WS ingress
	// connections racing a reconnect right as the engine recovers would.
	var wg sync.WaitGroup
	sessions := make([]backend.SandboxSession, n)
	oks := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessions[i], oks[i] = be.Adopt(context.Background(), runtimeID)
		}(i)
	}

	// Give every one of the n goroutines above time to actually reach
	// (or be correctly turned away before reaching) the blocking point in
	// ContainerAttachFunc — no goroutine does anything else that blocks,
	// so this is generous rather than a tight race.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&attachAttempts) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(releaseAttach)
	wg.Wait()

	for i := range n {
		if !oks[i] || sessions[i] != sess {
			t.Errorf("concurrent Adopt %d = (%v, %v), want (sess, true)", i, sessions[i], oks[i])
		}
	}
	if got := atomic.LoadInt32(&maxConcurrentAttachers); got != 1 {
		t.Errorf("max concurrent re-attach callers inside ContainerAttach = %d, want exactly 1 (dedup broken: more than one of the %d concurrent cache hits fired its own ContainerAttach at once)", got, n)
	}
	if got := atomic.LoadInt32(&attachAttempts); got != 2 {
		t.Errorf("ContainerAttach calls = %d, want exactly 2 (1 initial failure + 1 re-attach deduped across %d concurrent cache hits)", got, n)
	}
	if _, _, cancel, subOK, _ := sess.Subscribe(); !subOK {
		t.Error("Subscribe ok=false after a concurrent re-attach that should have succeeded")
	} else {
		cancel()
	}
}

// TestContainerBackend_Adopt_ReattachAfterMidStreamDropDoesNotDuplicateTranscript
// pins B1 from the Opus review of PR #857 (2nd round): reattachIfLost must
// NOT unconditionally request Logs:true on re-attach.
//
// running&&!attached (reattachIfLost's own gate) is reached by TWO
// different routes, and TestContainerBackend_Adopt_CacheHitReattachesAfterEngineRecovers
// above already pins the first one (doAdopt's own first attach attempt
// failed — this session's transcript is still empty, so replaying the
// container's pre-existing logs via Logs:true is correct). This test pins
// the SECOND, different route: a previously SUCCESSFUL attach whose
// readLoop ended while the container kept running (an engine/socket-proxy
// hiccup, or a live-restore daemon restart) — this session's transcript
// already holds everything the container produced up to that point.
// Requesting Logs:true here would make the engine replay that SAME
// history a second time, landing a literal duplicate in the transcript
// (and, for a Launched session, the on-disk spool file — the one thing
// `boid job log` can still read after ContainerRemove). Measured directly
// against a first-draft version of this fix before the len(s.transcript)
// check existed: the transcript came back as "EARLY-OUTPUT\nEARLY-OUTPUT\n"
// after a reattach that should have produced it exactly once.
func TestContainerBackend_Adopt_ReattachAfterMidStreamDropDoesNotDuplicateTranscript(t *testing.T) {
	var connMu sync.Mutex
	var attachConns []*fakeAttachConn
	var attachLogsFlags []bool

	api := &fakeDockerAPI{
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			connMu.Lock()
			c := newFakeAttachConn()
			attachConns = append(attachConns, c)
			attachLogsFlags = append(attachLogsFlags, options.Logs)
			connMu.Unlock()
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(c, "")}, nil
		},
		// Block forever: the container stays genuinely running across the
		// mid-stream drop and the reattach below — only the attach
		// connection itself drops and recovers.
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: make(chan container.WaitResponse), Error: make(chan error)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-midstream-drop", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-midstream-drop"})

	connMu.Lock()
	gen1 := attachConns[0]
	connMu.Unlock()
	gen1.feedFrame(1, []byte("EARLY-OUTPUT\n"))
	waitForTranscriptContaining(t, sess, "EARLY-OUTPUT")

	// Mid-stream drop: the attach connection ends (EOF) while the
	// container is still genuinely running (ContainerWaitFunc above never
	// resolves).
	gen1.Close()
	waitForSubscribeNotOK(t, sess)

	// Cache-hit Adopt must best-effort re-attach.
	sess2, ok := be.Adopt(context.Background(), sess.ID())
	if !ok || sess2 != sess {
		t.Fatalf("Adopt after mid-stream drop = (%v, %v), want the same cached session with ok=true", sess2, ok)
	}

	connMu.Lock()
	gotConns := len(attachConns)
	var logsFlag bool
	var gen2 *fakeAttachConn
	if gotConns >= 2 {
		logsFlag = attachLogsFlags[1]
		gen2 = attachConns[1]
	}
	connMu.Unlock()
	if gotConns != 2 {
		t.Fatalf("ContainerAttach calls = %d, want exactly 2 (initial Launch attach + one reattach)", gotConns)
	}

	if logsFlag {
		t.Error("re-attach after a mid-stream drop (transcript already non-empty) requested Logs:true; want false to avoid re-replaying already-captured output")
	}

	gen2.feedFrame(1, []byte("AFTER-RECOVERY\n"))
	waitForTranscriptContaining(t, sess, "AFTER-RECOVERY")

	snapshot, _, cancel, _, _ := sess.Subscribe()
	cancel()
	if n := bytes.Count(snapshot, []byte("EARLY-OUTPUT")); n != 1 {
		t.Errorf("transcript contains %q %d times, want exactly 1 (must not duplicate on reattach), snapshot = %q", "EARLY-OUTPUT", n, snapshot)
	}
	if !bytes.Contains(snapshot, []byte("AFTER-RECOVERY")) {
		t.Errorf("transcript = %q, want it to also contain the post-recovery output", snapshot)
	}
}

// TestContainerBackend_Adopt_ReattachClosesThePreviousGenerationsConnection
// pins N1 from the Opus review of PR #857: attach() must explicitly close
// the PREVIOUS generation's hijack connection when reattachIfLost installs
// a new one, rather than merely overwriting s.hijack and letting the old
// value become unreachable. A Read() returning an error (the remote engine
// hanging up) does not itself release the underlying connection/fd on our
// side — against a real docker engine that is a live TCP/unix socket that
// stays open until something calls Close() on it — so every mid-stream-
// drop reattach leaked one connection before this fix. closeConn()
// (waitLoop's own cleanup) does not cover this case either: it always
// closes whatever s.hijack CURRENTLY is, and by the time the container
// actually exits s.hijack already points at the NEW generation, not the
// abandoned one.
//
// The drop is simulated via a raw pipe-side closure (gen1.outW.
// CloseWithError), deliberately NOT via fakeAttachConn's own Close()
// wrapper: calling that wrapper would itself set gen1.closed = true,
// making it impossible to tell "the test's own setup closed this" apart
// from "attach()'s new N1 fix closed this". A real remote hangup has
// exactly this shape too — the local Read() errors out on its own, with
// nothing on the local side having called Close() yet.
func TestContainerBackend_Adopt_ReattachClosesThePreviousGenerationsConnection(t *testing.T) {
	var connMu sync.Mutex
	var attachConns []*fakeAttachConn

	api := &fakeDockerAPI{
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			connMu.Lock()
			c := newFakeAttachConn()
			attachConns = append(attachConns, c)
			connMu.Unlock()
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(c, "")}, nil
		},
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: make(chan container.WaitResponse), Error: make(chan error)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-stale-conn", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-stale-conn"})

	connMu.Lock()
	gen1 := attachConns[0]
	connMu.Unlock()

	// Raw pipe-side closure: readLoop's Read() errors out and ends, but
	// gen1 itself is not thereby marked closed — exactly the gap this fix
	// closes.
	gen1.outW.CloseWithError(io.ErrClosedPipe) //nolint:errcheck
	waitForSubscribeNotOK(t, sess)

	if gen1.isClosed() {
		t.Fatal("test setup bug: gen1 should not be marked closed by the raw pipe-side closure alone")
	}

	sess2, ok := be.Adopt(context.Background(), sess.ID())
	if !ok || sess2 != sess {
		t.Fatalf("Adopt after simulated drop = (%v, %v), want the same cached session with ok=true", sess2, ok)
	}

	if !gen1.isClosed() {
		t.Error("the previous generation's connection was never explicitly closed on reattach — this leaks a connection (and, against a real docker engine, its underlying fd) on every mid-stream-drop reattach")
	}
}

// TestContainerSession_CloseInput_WorksAgainAfterReattach pins N6 from the
// Opus review of PR #857: CloseInput's half-close must still work against
// the SECOND (re-attached) generation's connection. A single session-
// lifetime sync.Once (the pre-fix shape) would let the first CloseInput
// call consume the Once forever, silently no-op'ing every later call —
// including one made after a reattach installed a brand new connection
// that has never itself been half-closed.
func TestContainerSession_CloseInput_WorksAgainAfterReattach(t *testing.T) {
	var connMu sync.Mutex
	var attachConns []*fakeAttachConn

	api := &fakeDockerAPI{
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			connMu.Lock()
			c := newFakeAttachConn()
			attachConns = append(attachConns, c)
			connMu.Unlock()
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(c, "")}, nil
		},
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: make(chan container.WaitResponse), Error: make(chan error)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-closeinput-reattach", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-closeinput-reattach"})

	connMu.Lock()
	gen1 := attachConns[0]
	connMu.Unlock()

	if err := sess.CloseInput(); err != nil {
		t.Fatalf("first CloseInput: %v", err)
	}
	if got := atomic.LoadInt32(&gen1.closeWrites); got != 1 {
		t.Fatalf("gen1 CloseWrite calls = %d, want 1", got)
	}

	// Mid-stream drop + reattach — see
	// TestContainerBackend_Adopt_ReattachClosesThePreviousGenerationsConnection's
	// own doc comment for why a raw pipe-side closure (not gen1.Close())
	// is used to simulate it.
	gen1.outW.CloseWithError(io.ErrClosedPipe) //nolint:errcheck
	waitForSubscribeNotOK(t, sess)
	sess2, ok := be.Adopt(context.Background(), sess.ID())
	if !ok || sess2 != sess {
		t.Fatalf("Adopt after simulated drop = (%v, %v), want the same cached session with ok=true", sess2, ok)
	}

	connMu.Lock()
	gotConns := len(attachConns)
	var gen2 *fakeAttachConn
	if gotConns >= 2 {
		gen2 = attachConns[1]
	}
	connMu.Unlock()
	if gotConns != 2 {
		t.Fatalf("attach calls = %d, want 2 (initial Launch attach + one reattach)", gotConns)
	}

	if err := sess.CloseInput(); err != nil {
		t.Fatalf("second CloseInput (after reattach): %v", err)
	}
	if got := atomic.LoadInt32(&gen2.closeWrites); got != 1 {
		t.Errorf("gen2 CloseWrite calls = %d, want 1 (CloseInput must still work against the re-attached generation, not silently no-op forever after the first call)", got)
	}
}

func TestContainerBackend_ReapOrphans_LabelBasedDestroy(t *testing.T) {
	api := &fakeDockerAPI{
		ContainerListFunc: func(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
			// Filter-aware, because ReapOrphans issues two independent
			// enumerations as of PR5 (docs/plans/workspace-home-volume-persistence.md
			// 論点 c §D5) and a fake that answered both with the same items
			// would have the workspace-init sweep re-remove every job
			// container — the very cross-contamination the separate label
			// keys exist to prevent.
			if !options.Filters["label"][labelJobID] {
				return client.ContainerListResult{}, nil
			}
			return client.ContainerListResult{Items: []container.Summary{
				{ID: "orphan-1", Labels: map[string]string{labelJobID: "job-a"}},
				{ID: "orphan-2", Labels: map[string]string{labelJobID: "job-b"}},
			}}, nil
		},
		ContainerRemoveFunc: func(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			if containerID == "orphan-2" {
				return client.ContainerRemoveResult{}, context.DeadlineExceeded
			}
			return client.ContainerRemoveResult{}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	report, err := be.ReapOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(report.ReapedJobIDs) != 1 || report.ReapedJobIDs[0] != "job-a" {
		t.Errorf("ReapedJobIDs = %v, want [job-a]", report.ReapedJobIDs)
	}
	if len(report.FailedJobIDs) != 1 || report.FailedJobIDs[0] != "job-b" {
		t.Errorf("FailedJobIDs = %v, want [job-b]", report.FailedJobIDs)
	}

	// Two enumerations: the job sweep (§決定 6's global boid.job_id filter)
	// and, since PR5, the workspace-init sweep on its own label. They are
	// deliberately not one query — see reapOrphanWorkspaceInitContainers.
	if len(api.listFilters) != 2 {
		t.Fatalf("ContainerList calls = %d, want 2 (the job sweep and the workspace-init sweep)", len(api.listFilters))
	}
	if _, ok := api.listFilters[0]["label"][labelJobID]; !ok {
		t.Errorf("ContainerList filter = %+v, want a %q label filter (§決定 6 global filter)", api.listFilters[0], labelJobID)
	}
	if _, ok := api.listFilters[1]["label"][dockerres.LabelWorkspaceInit]; !ok {
		t.Errorf("second ContainerList filter = %+v, want a %q label filter", api.listFilters[1], dockerres.LabelWorkspaceInit)
	}
	if api.volumeListCalls == 0 {
		t.Error("VolumeList was not called (ReapOrphans should sweep volumes too)")
	}
	if api.networkListCalls == 0 {
		t.Error("NetworkList was not called (ReapOrphans should sweep networks too)")
	}
}

// TestContainerBackend_ReapOrphans_ScopesByInstallID pins [Blocker 5, PR7
// codex review]: when ContainerBackendOptions.InstallID is set, ReapOrphans
// must only destroy resources whose boid.install_id label matches — a
// second installation's live containers sharing the same docker engine
// (distinct install_id, no boid.install_id label at all, or a stale/foreign
// label value) must be left completely alone.
func TestContainerBackend_ReapOrphans_ScopesByInstallID(t *testing.T) {
	api := &fakeDockerAPI{
		ContainerListFunc: func(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
			if !options.Filters["label"][labelJobID] {
				return client.ContainerListResult{}, nil
			}
			return client.ContainerListResult{Items: []container.Summary{
				{ID: "mine-1", Labels: map[string]string{labelJobID: "job-mine", labelInstallID: "install-A"}},
				{ID: "theirs-1", Labels: map[string]string{labelJobID: "job-theirs", labelInstallID: "install-B"}},
				{ID: "unlabeled-install-1", Labels: map[string]string{labelJobID: "job-unlabeled"}},
			}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-A"})

	report, err := be.ReapOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	if len(api.removeIDs) != 1 || api.removeIDs[0] != "mine-1" {
		t.Fatalf("ContainerRemove calls = %v, want exactly [mine-1] (other installations' containers must never be touched)", api.removeIDs)
	}
	if len(report.ReapedJobIDs) != 1 || report.ReapedJobIDs[0] != "job-mine" {
		t.Errorf("ReapedJobIDs = %v, want [job-mine]", report.ReapedJobIDs)
	}
	for _, jobID := range report.ReapedJobIDs {
		if jobID == "job-theirs" || jobID == "job-unlabeled" {
			t.Errorf("ReapedJobIDs = %v must not include a foreign-installation job", report.ReapedJobIDs)
		}
	}
}

// TestContainerBackend_ReapOrphans_EmptyInstallID_FallsBackToGlobalFilter
// pins the pre-PR6 / test-wiring degrade: an empty InstallID (the
// zero-value ContainerBackendOptions every pre-Blocker-5 caller/test uses)
// must reap every boid.job_id-labeled container regardless of its
// boid.install_id label, unchanged from before this fix.
func TestContainerBackend_ReapOrphans_EmptyInstallID_FallsBackToGlobalFilter(t *testing.T) {
	api := &fakeDockerAPI{
		ContainerListFunc: func(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
			if !options.Filters["label"][labelJobID] {
				return client.ContainerListResult{}, nil
			}
			return client.ContainerListResult{Items: []container.Summary{
				{ID: "orphan-1", Labels: map[string]string{labelJobID: "job-a", labelInstallID: "install-A"}},
				{ID: "orphan-2", Labels: map[string]string{labelJobID: "job-b"}},
			}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	if _, err := be.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(api.removeIDs) != 2 {
		t.Fatalf("ContainerRemove calls = %v, want both orphan-1 and orphan-2 removed (no install_id configured)", api.removeIDs)
	}
}

func TestContainerSession_Signal_ForwardsSIGUSR1ToPID1(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-signal", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-signal"})

	if err := sess.Signal(context.Background(), syscall.SIGUSR1); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	if len(api.killIDs) != 1 || api.killIDs[0] != sess.ID() {
		t.Fatalf("ContainerKill calls = %v, want exactly [%s]", api.killIDs, sess.ID())
	}
	// docker kill --signal=SIGUSR1 targets the container's PID 1
	// (docker-init, §決定 3), which forwards to the entrypoint process —
	// this is that "docker kill --signal=..." call, not a raw process-group
	// kill the way the userns backend's LocalRuntime.Signal is.
	if got := api.killCalls[0].Signal; got != "SIGUSR1" {
		t.Errorf("ContainerKill signal = %q, want SIGUSR1", got)
	}
}

func TestContainerSession_Subscribe_MultipleSubscribersReceiveSameStream(t *testing.T) {
	var conn *fakeAttachConn
	var connMu sync.Mutex

	api := &fakeDockerAPI{
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			connMu.Lock()
			conn = newFakeAttachConn()
			c := conn
			connMu.Unlock()
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(c, "")}, nil
		},
		// Block forever: this test only exercises Subscribe/fan-out, not
		// exit handling. Without this override the default ContainerWait
		// resolves immediately, and waitLoop's post-exit closeConn (see its
		// doc comment) races with — and can win against — this test's own
		// feedFrame call below.
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: make(chan container.WaitResponse), Error: make(chan error)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-sub", Argv: []string{"true"}, TTY: false}, backend.LaunchOptions{JobID: "job-sub"})

	_, ch1, cancel1, ok1, _ := sess.Subscribe()
	_, ch2, cancel2, ok2, _ := sess.Subscribe()
	if !ok1 || !ok2 {
		t.Fatalf("Subscribe ok = (%v, %v), want (true, true)", ok1, ok2)
	}
	defer cancel1()
	defer cancel2()

	connMu.Lock()
	c := conn
	connMu.Unlock()
	c.feedFrame(1, []byte("hello from container"))

	want := "hello from container"
	got1 := readChunkTimeout(t, ch1)
	got2 := readChunkTimeout(t, ch2)
	if string(got1) != want {
		t.Errorf("subscriber 1 got %q, want %q", got1, want)
	}
	if string(got2) != want {
		t.Errorf("subscriber 2 got %q, want %q", got2, want)
	}

	// A late Subscribe (after the fact) must see the same bytes in its
	// snapshot.
	snapshot, _, cancel3, _, _ := sess.Subscribe()
	cancel3()
	if !strings.Contains(string(snapshot), want) {
		t.Errorf("late-subscribe snapshot = %q, want it to contain %q", snapshot, want)
	}
}

func readChunkTimeout(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case chunk := <-ch:
		return chunk
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a transcript chunk")
		return nil
	}
}

// waitForTranscriptContaining polls sess.Subscribe's snapshot until it
// contains want, bounded so a broken readLoop wiring fails the test instead
// of hanging it. Used by reattach tests that need to be sure readLoop has
// actually consumed a fed frame into the transcript before moving on to the
// next phase (closing the connection, re-attaching, ...).
func waitForTranscriptContaining(t *testing.T, sess backend.SandboxSession, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, _, cancel, _, _ := sess.Subscribe()
		cancel()
		if bytes.Contains(snapshot, []byte(want)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for transcript to contain %q; last snapshot = %q", want, snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForSubscribeNotOK polls sess.Subscribe until it reports ok=false,
// bounded the same way waitForTranscriptContaining is. Used to force a
// just-closed generation's readLoop to actually finish (attached flips to
// false) before the test moves on to a re-attach — see
// TestContainerBackend_Adopt_CacheHitReattachesAfterEngineRecovers's own
// "ending the RE-ATTACHED generation's stream" comment for why this
// synchronization matters (it forces readLoop's deferred close(readDone)
// to run inside the test's own lifetime rather than asynchronously after
// it).
func waitForSubscribeNotOK(t *testing.T, sess backend.SandboxSession) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, _, ok, _ := sess.Subscribe(); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Subscribe to report ok=false")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestContainerSession_Wait_SingleOwnerFanOut(t *testing.T) {
	waitCh := make(chan container.WaitResponse, 1)
	waitCh <- container.WaitResponse{StatusCode: 7}

	api := &fakeDockerAPI{
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			// A small delay makes the two concurrent Wait() callers below
			// genuinely both be blocked on <-s.done when the single
			// underlying ContainerWait resolves, rather than trivially
			// racing one call already being done before the second starts.
			time.Sleep(20 * time.Millisecond)
			return client.ContainerWaitResult{Result: waitCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-wait", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-wait"})

	var wg sync.WaitGroup
	results := make([]backend.RuntimeExit, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = sess.Wait(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Wait() call %d returned error: %v", i, err)
		}
		if results[i].ExitCode != 7 {
			t.Errorf("Wait() call %d ExitCode = %d, want 7", i, results[i].ExitCode)
		}
	}
	if got := api.waitCallCount(); got != 1 {
		t.Errorf("ContainerWait was called %d times, want exactly 1 (§決定 7 single-owner fan-out)", got)
	}
}

// TestContainerSession_WaitLoop_EngineErrorInTheWaitResponseFailsTheJob pins
// that waitLoop treats an engine-reported wait error the same as the
// ContainerWait API-call failure path right below it (case err :=
// <-waitRes.Error), on the job dispatch path — the counterpart of
// TestContainerBackend_RunWorkspaceInit_EngineErrorInTheWaitResponseFailsTheRun
// (container_backend_workspace_init_test.go), which already pins this for
// the workspace-init path via the shared waitResponseEngineError helper.
//
// container.WaitResponse carries two independent facts and only one of them
// is an exit status: when the engine cannot report one — the runtime failed
// to wait, the shim died, the container never started — it answers with
// StatusCode: 0 and a non-nil Error, which is exactly the shape of a
// successful exit if only StatusCode is read. This session's own ExitCode is
// therefore wrong on its own terms whether or not any particular caller
// happens to paper over it afterward — see the doc comment on this case in
// waitLoop itself for how Runner.watchRuntime's independent 0→1 coercion
// relates to (but does not substitute for) this.
//
// Asserted as == 1, not just != 0: the exact value matters because it pins
// parity with the adjacent case's existing exitCode = 1, not merely "some
// nonzero code" (which an unrelated regression, e.g. exitCode = 2, would
// still satisfy).
func TestContainerSession_WaitLoop_EngineErrorInTheWaitResponseFailsTheJob(t *testing.T) {
	const engineMessage = "runtime wait failed"
	api := &fakeDockerAPI{
		ContainerWaitFunc: waitResponding(container.WaitResponse{
			// Exactly the shape that makes the omission invisible: the
			// status code is the success value, and the only evidence of
			// the failure is in Error.
			StatusCode: 0,
			Error:      &container.WaitExitError{Message: engineMessage},
		}),
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-engine-error", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-engine-error"})

	exit, err := sess.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.ExitCode != 1 {
		t.Errorf("ExitCode = %d for a wait the engine answered with an error (%q), want 1 (parity with the adjacent <-waitRes.Error failure path's existing exitCode = 1)", exit.ExitCode, engineMessage)
	}
}

// TestContainerSession_WaitLoop_DrainsAttachBeforeClosingConn pins Major 3
// from the PR5 review: once ContainerWait resolves (the container process
// exited), waitLoop must let readLoop drain any output still in flight on
// the attach connection before closing it, rather than closing immediately.
// This simulates a final burst of output arriving on the attach stream
// after the exit code is already known but before the stream itself is
// closed (EOF) — the pre-fix code closed the connection right after
// ContainerWait resolved, which raced against (and could drop) exactly
// this burst.
func TestContainerSession_WaitLoop_DrainsAttachBeforeClosingConn(t *testing.T) {
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
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-drain", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-drain"})

	// The container process exits (ContainerWait resolves)...
	waitCh <- container.WaitResponse{StatusCode: 0}

	// ...but its final output burst is still in flight on the attach
	// connection. Give waitLoop a moment to observe the exit and enter its
	// drain-wait before feeding it: under the pre-fix behavior (close
	// immediately on exit) this frame would race against — and lose to —
	// that close.
	time.Sleep(50 * time.Millisecond)
	finalBurst := []byte("final burst of output written right at exit")
	conn.feedFrame(1, finalBurst)
	conn.Close() // EOF: readLoop finishes draining naturally.

	exit, err := sess.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", exit.ExitCode)
	}

	snapshot, _, cancel, subOK, finished := sess.Subscribe()
	cancel()
	if !bytes.Contains(snapshot, finalBurst) {
		t.Errorf("transcript = %q, want it to contain the final burst %q (drained before close)", snapshot, finalBurst)
	}
	// Regression pin alongside B1/B2's own new tests (Opus review of PR
	// #857): a genuinely, cleanly exited container must still report
	// ok=false, finished=true — the pre-existing "job done" case B2's
	// finished field must not accidentally flip for.
	if subOK {
		t.Error("Subscribe ok=true after the container has genuinely exited; want false")
	}
	if !finished {
		t.Error("Subscribe finished=false after the container has genuinely exited; want true")
	}
}

// TestContainerSession_WaitLoop_SkipsDrainSelectWhenNeverAttached is a
// regression pin requested by the Opus review of PR #857 (N4): a session
// that was never successfully attached (doAdopt's own best-effort attach
// failed, and no later cache-hit reattachIfLost fixed it before the
// container happened to exit) must tear down promptly when the container
// exits — Wait() must return in well under attachDrainGracePeriod, not
// incur an artificial ~500ms stall waiting on a reader that was never
// started.
//
// Caveat, checked directly against this file's own code rather than
// asserted from the outside (worth recording here since it shapes what
// this test can and cannot prove): mutating waitLoop's `if attached {...}
// else {...}` split to unconditionally enter the select branch does NOT
// make this test fail. That is not a gap in this test — it is a provable
// consequence of an invariant every attached=false-setting site in this
// file maintains: attach()'s own error path closes its freshly-allocated
// readDone BEFORE it publishes attached=false, and readLoop's deferred
// close(readDone) likewise always accompanies (and is the only writer of)
// the transition to attached=false. Both attached and readDone are always
// read together under one s.mu critical section (here and in Subscribe),
// so any snapshot observing attached=false is guaranteed to be paired with
// an ALREADY-CLOSED readDone — meaning entering the select unconditionally
// still returns immediately via its `case <-readDone` arm. The `else`
// branch is therefore a clarity/directness simplification over relying on
// that indirection (see waitLoop's own doc comment) rather than a
// currently-reachable bug fix — this test pins the property that actually
// matters (prompt teardown) rather than a mutation this design makes
// unreachable.
func TestContainerSession_WaitLoop_SkipsDrainSelectWhenNeverAttached(t *testing.T) {
	waitCh := make(chan container.WaitResponse, 1)
	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					State:  &container.State{Running: true},
					Config: &container.Config{},
				},
			}, nil
		},
		ContainerAttachFunc: func(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			return client.ContainerAttachResult{}, errors.New("engine unreachable")
		},
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: waitCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess, ok := be.Adopt(context.Background(), "never-attached-container")
	if !ok {
		t.Fatal("Adopt returned ok=false")
	}

	start := time.Now()
	waitCh <- container.WaitResponse{StatusCode: 0}
	if _, err := sess.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= attachDrainGracePeriod {
		t.Errorf("Wait took %v (>= attachDrainGracePeriod %v); a never-attached session's waitLoop should skip the drain-select entirely, not merely fall through it", elapsed, attachDrainGracePeriod)
	}
}

// TestContainerSession_WaitLoop_RunsDiagnosticsCollectorBeforeRemove pins
// Major 4 from the PR5 review (§決定 7's "診断回収 → job fallback 処理 →
// resource remove" ordering contract): once a container exits,
// ContainerBackendOptions.DiagnosticsCollector — when configured — must run
// to completion strictly before ContainerRemove is called. It also pins
// that the normal-exit remove path no longer forces removal unconditionally
// (Force is reserved for the error-retry path).
func TestContainerSession_WaitLoop_RunsDiagnosticsCollectorBeforeRemove(t *testing.T) {
	var mu sync.Mutex
	var order []string

	waitCh := make(chan container.WaitResponse, 1)
	waitCh <- container.WaitResponse{StatusCode: 0}

	api := &fakeDockerAPI{
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			return client.ContainerWaitResult{Result: waitCh, Error: make(chan error, 1)}
		},
		ContainerRemoveFunc: func(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			mu.Lock()
			order = append(order, "remove")
			mu.Unlock()
			if options.Force {
				t.Errorf("ContainerRemove Force = true on the normal exit path, want false (Major 4: force is reserved for the error-retry path)")
			}
			return client.ContainerRemoveResult{}, nil
		},
	}

	collectorDone := make(chan struct{})
	opts := ContainerBackendOptions{
		DiagnosticsCollector: func(ctx context.Context, containerID string, exit backend.RuntimeExit) {
			mu.Lock()
			order = append(order, "collect")
			mu.Unlock()
			// Simulate collector work taking a moment, so a pre-fix
			// (immediate-remove) implementation would race ahead of it.
			time.Sleep(30 * time.Millisecond)
			close(collectorDone)
		},
	}
	be := NewContainerBackend(api, opts)
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-diag", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-diag"})

	if _, err := sess.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	select {
	case <-collectorDone:
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostics collector was not invoked")
	}

	// ContainerRemove runs asynchronously to Wait's return; poll briefly
	// for it to show up rather than asserting immediately.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for ContainerRemove to be called after the diagnostics collector ran")
		case <-time.After(10 * time.Millisecond):
		}
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "collect" || got[1] != "remove" {
		t.Fatalf("call order = %v, want [collect remove] (§決定 7: diagnostics before resource teardown)", got)
	}
}

// TestContainerSession_TranscriptSpool_SurvivesContainerRemove pins §決定8's
// PR7 transcript-persistence contract: `boid job log` (ReadTranscript/
// StatTranscript, transcript.go) must be able to read a container-backend
// job's full transcript even after the container itself — and, in a real
// deploy, `docker logs` along with it — is gone. With
// ContainerBackendOptions.RuntimeDir set, Launch must have spooled every
// appendTranscript chunk to <runtimeDir>/<containerID>/transcript.log by
// the time Wait returns (waitLoop closes the spool file before closing
// s.done — see its own doc comment), so no polling/timing dance is needed
// here: Wait returning is itself the synchronization point.
func TestContainerSession_TranscriptSpool_SurvivesContainerRemove(t *testing.T) {
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
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-spool", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-spool"})

	want := "hello disk spool, surviving container remove"
	conn.feedFrame(1, []byte(want))
	waitCh <- container.WaitResponse{StatusCode: 0}
	conn.Close() // EOF: readLoop finishes draining naturally.

	exit, err := sess.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.TranscriptPath == "" {
		t.Error("RuntimeExit.TranscriptPath is empty, want the spool path (watchRuntime's silent-exit diagnostics rely on this)")
	}

	// waitLoop's own ContainerRemove call runs asynchronously to Wait's
	// return, so — unlike the spool file, whose close is synchronized with
	// s.done above — poll briefly for it, purely to demonstrate the
	// transcript stays readable AFTER removal, not merely before it.
	deadline := time.After(2 * time.Second)
	for api.removeCallCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for ContainerRemove to be called")
		case <-time.After(10 * time.Millisecond):
		}
	}

	data, err := ReadTranscript(runtimeDir, sess.ID())
	if err != nil {
		t.Fatalf("ReadTranscript after container remove: %v", err)
	}
	if !bytes.Contains(data, []byte(want)) {
		t.Errorf("transcript = %q, want it to contain %q", data, want)
	}

	fi, err := StatTranscript(runtimeDir, sess.ID())
	if err != nil {
		t.Fatalf("StatTranscript after container remove: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("StatTranscript reports a 0-byte transcript, want it to reflect the spooled content")
	}
}

func TestContainerBackend_ImageSelection_UsesSpecContainerImageOrDefault(t *testing.T) {
	t.Run("no override uses the configured default image", func(t *testing.T) {
		api := &fakeDockerAPI{
			ImageInspectFunc: func(ctx context.Context, imageRef string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
				return imageInspectResultWithLabel(""), nil
			},
		}
		be := NewContainerBackend(api, ContainerBackendOptions{DefaultImage: "boid-runner:v9"})
		mustLaunch(t, be, sandbox.Spec{ID: "job-default-image", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-default-image"})

		if len(api.createCalls) != 1 {
			t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
		}
		if got, want := api.createCalls[0].Config.Image, "boid-runner:v9"; got != want {
			t.Errorf("Config.Image = %q, want %q", got, want)
		}
	})

	t.Run("override with a valid label is used", func(t *testing.T) {
		api := &fakeDockerAPI{
			ImageInspectFunc: func(ctx context.Context, imageRef string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
				return imageInspectResultWithLabel(boidRunnerProtocolVersion), nil
			},
		}
		be := NewContainerBackend(api, ContainerBackendOptions{DefaultImage: "boid-runner:v9"})
		mustLaunch(t, be, sandbox.Spec{ID: "job-override-image", Argv: []string{"true"}, ContainerImage: "ghcr.io/acme/boid-runner-custom:v1"},
			backend.LaunchOptions{JobID: "job-override-image"})

		if len(api.createCalls) != 1 {
			t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
		}
		if got, want := api.createCalls[0].Config.Image, "ghcr.io/acme/boid-runner-custom:v1"; got != want {
			t.Errorf("Config.Image = %q, want %q", got, want)
		}
	})
}

// TestRunner_Backend_DrivesContainerBackendThroughSignalSeam is the
// container-backend sibling of runner_backend_wiring_test.go's
// TestRunner_SignalJobRuntime_RoutesThroughBackendAdoptToSessionSignal (PR1):
// that test proves the Runner.Backend DI seam works for *some*
// backend.SandboxBackend; this one plugs a real containerBackend into it and
// confirms `boid agent stop`'s SIGUSR1 delivery
// (NotifyTask → Runner.SignalJobRuntime) reaches an actual docker kill call
// — the "内部フラグ/テスト専用で containerBackend の動作確認" TODO item, driven
// through Runner rather than the backend directly.
func TestRunner_Backend_DrivesContainerBackendThroughSignalSeam(t *testing.T) {
	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{State: &container.State{Running: true}, Config: &container.Config{}},
			}, nil
		},
	}
	r := &Runner{Backend: NewContainerBackend(api, ContainerBackendOptions{})}

	r.SignalJobRuntime("running-container-id", syscall.SIGUSR1)

	if len(api.killIDs) != 1 || api.killIDs[0] != "running-container-id" {
		t.Fatalf("ContainerKill calls = %v, want exactly [running-container-id]", api.killIDs)
	}
	if got := api.killCalls[0].Signal; got != "SIGUSR1" {
		t.Errorf("ContainerKill signal = %q, want SIGUSR1", got)
	}
}

// TestContainerBackend_ImagePullAlways_RevalidatesAfterPull pins Major 2
// from the PR5 review: with ImagePullAlways, resolveImage's pre-pull
// ImageInspect (used for the presence check) must not be reused to
// validate the override-image label after pulling — the pull can replace
// the image (a moved tag), so a fresh post-pull ImageInspect is required.
// This simulates exactly that: the image is present pre-pull *with* the
// required label, but the pull silently swaps it for one *without* the
// label — Launch must still reject the override.
func TestContainerBackend_ImagePullAlways_RevalidatesAfterPull(t *testing.T) {
	var inspectCalls int
	api := &fakeDockerAPI{
		ImageInspectFunc: func(ctx context.Context, imageRef string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
			inspectCalls++
			if inspectCalls == 1 {
				return imageInspectResultWithLabel(boidRunnerProtocolVersion), nil
			}
			return imageInspectResultWithLabel(""), nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{PullPolicy: ImagePullAlways})

	_, err := be.Launch(context.Background(), sandbox.Spec{
		ID:             "job-pull-revalidate",
		Argv:           []string{"true"},
		ContainerImage: "ghcr.io/acme/boid-runner-custom:v1",
	}, backend.LaunchOptions{JobID: "job-pull-revalidate"})
	if err == nil {
		t.Fatal("Launch succeeded despite the post-pull image losing the boid_runner_protocol label, want an error")
	}
	if !strings.Contains(err.Error(), boidRunnerProtocolLabel) {
		t.Errorf("Launch error = %q, want it to mention %q", err.Error(), boidRunnerProtocolLabel)
	}
	if inspectCalls != 2 {
		t.Errorf("ImageInspect calls = %d, want exactly 2 (pre-pull presence check + post-pull revalidation)", inspectCalls)
	}
	if len(api.pullRefs) != 1 {
		t.Errorf("ImagePull calls = %d, want exactly 1", len(api.pullRefs))
	}
	if len(api.createCalls) != 0 {
		t.Errorf("ContainerCreate was called %d times, want 0 (rejected before create)", len(api.createCalls))
	}
}

// TestBoidRunnerProtocolLabel_IsBakedIntoTheImage is the other end of the
// override gate: resolveImage rejects any override image that does not carry
// boid.runner_protocol=v1, so unless the shared base image actually declares
// it, EVERY override is rejected — including boid's own `boid-runner:latest`
// passed explicitly. That was the state through 2026-07-28: the constants
// existed, build/container/Dockerfile had no LABEL instruction at all, and a
// live image's labels were measured as {"io.buildah.version":"1.33.7"}. The
// code's own comment said the gate was harmless "because containerBackend is
// not wired into production dispatch as of PR5", a premise the container
// cutover removed without anyone revisiting the conclusion.
//
// What this pins is that the Dockerfile and the constants agree. It cannot
// pin that a BUILT image carries the label — an instruction can be added to
// the wrong build stage and this still passes — so the built image is
// checked separately, against a real `docker image inspect`, in
// .github/workflows/blackbox-e2e.yml's image-build job.
func TestBoidRunnerProtocolLabel_IsBakedIntoTheImage(t *testing.T) {
	path := filepath.Join("..", "..", "build", "container", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	want := fmt.Sprintf("LABEL %s=%q", boidRunnerProtocolLabel, boidRunnerProtocolVersion)
	if !strings.Contains(string(data), want) {
		t.Errorf("build/container/Dockerfile does not contain %s\n"+
			"Every workspace container_image override is rejected without it, because resolveImage requires that exact label/value pair.", want)
	}
}

func TestContainerBackend_ImageOverride_RequiresTheProtocolLabel(t *testing.T) {
	tests := []struct {
		name       string
		labelValue string // "" omits the label entirely
	}{
		{name: "missing label", labelValue: ""},
		{name: "wrong label value", labelValue: "v0-not-a-real-version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeDockerAPI{
				ImageInspectFunc: func(ctx context.Context, imageRef string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
					return imageInspectResultWithLabel(tt.labelValue), nil
				},
			}
			be := NewContainerBackend(api, ContainerBackendOptions{})

			_, err := be.Launch(context.Background(), sandbox.Spec{
				ID:             "job-bad-override",
				Argv:           []string{"true"},
				ContainerImage: "untrusted/random-image:latest",
			}, backend.LaunchOptions{JobID: "job-bad-override"})
			if err == nil {
				t.Fatal("Launch succeeded with a non-boid-base-derived override image, want an error")
			}
			if !strings.Contains(err.Error(), boidRunnerProtocolLabel) {
				t.Errorf("Launch error = %q, want it to mention %q", err.Error(), boidRunnerProtocolLabel)
			}
			if len(api.createCalls) != 0 {
				t.Errorf("ContainerCreate was called %d times, want 0 (rejected before create)", len(api.createCalls))
			}
		})
	}
}

// TestContainerBackend_Launch_BoundsTheTeardownOnAFailedLaunch is the third
// occurrence of the defect codex round 2 named as Major 3 for the migration's
// extraction container, found by the grep it asked for.
//
// Launch tears its own half-built container down on three error paths (spool
// open, attach, start) and every one of them issued ContainerRemove on a bare
// context.Background(): no cancellation, and no deadline either. Against an
// engine socket that accepts a request and never answers, Launch never returns
// the error it already has — and since PR8, Dispatch holds this workspace's
// in-flight home registration across the whole of Launch, so a wedged teardown
// also refuses every later `boid workspace import-home` for that workspace with
// a message about a job that is not running.
//
// Dropping the caller's cancellation is still right (a job launch cancelled
// mid-flight is exactly when the teardown matters most, and inheriting the
// cancellation would leak the container). The missing half is the bound.
func TestContainerBackend_Launch_BoundsTheTeardownOnAFailedLaunch(t *testing.T) {
	restore := shrinkWorkspaceHomeContainerCleanupTimeout(t, 100*time.Millisecond)
	defer restore()

	removeCtx := make(chan context.Context, 4)
	api := &fakeDockerAPI{
		ContainerAttachFunc: func(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			return client.ContainerAttachResult{}, errors.New("attach refused")
		},
		ContainerRemoveFunc: func(ctx context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			removeCtx <- ctx
			<-ctx.Done() // an engine that accepted the request and never answers
			return client.ContainerRemoveResult{}, ctx.Err()
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := be.Launch(ctx, sandbox.Spec{ID: "job-wedged", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-wedged"})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("test wiring: this Launch was supposed to fail at attach")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Launch never returned: its teardown ContainerRemove is issued on a context with no deadline, so a " +
			"wedged engine socket pins the dispatch — and with it the workspace's in-flight home registration — forever")
	}

	select {
	case got := <-removeCtx:
		if _, ok := got.Deadline(); !ok {
			t.Error("the teardown removal was issued on a context with no deadline")
		}
		if errors.Is(got.Err(), context.Canceled) {
			t.Error("the teardown removal inherited the caller's cancellation, so a cancelled Launch leaks its container")
		}
	default:
		t.Fatal("ContainerRemove was never called: the half-built container is leaked")
	}
}
