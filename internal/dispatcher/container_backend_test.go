package dispatcher

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

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

func TestContainerBackend_ReapOrphans_LabelBasedDestroy(t *testing.T) {
	api := &fakeDockerAPI{
		ContainerListFunc: func(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
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

	if len(api.listFilters) != 1 {
		t.Fatalf("ContainerList calls = %d, want 1", len(api.listFilters))
	}
	if _, ok := api.listFilters[0]["label"][labelJobID]; !ok {
		t.Errorf("ContainerList filter = %+v, want a %q label filter (§決定 6 global filter)", api.listFilters[0], labelJobID)
	}
	if api.volumeListCalls == 0 {
		t.Error("VolumeList was not called (ReapOrphans should sweep volumes too)")
	}
	if api.networkListCalls == 0 {
		t.Error("NetworkList was not called (ReapOrphans should sweep networks too)")
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

	_, ch1, cancel1, ok1 := sess.Subscribe()
	_, ch2, cancel2, ok2 := sess.Subscribe()
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
	snapshot, _, cancel3, _ := sess.Subscribe()
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

	snapshot, _, cancel, _ := sess.Subscribe()
	cancel()
	if !bytes.Contains(snapshot, finalBurst) {
		t.Errorf("transcript = %q, want it to contain the final burst %q (drained before close)", snapshot, finalBurst)
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
	r := &Runner{Runtime: &ubFakeRuntime{}, Backend: NewContainerBackend(api, ContainerBackendOptions{})}

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

func TestContainerBackend_ImageOverride_RequiresBoidBaseDerivedLabel(t *testing.T) {
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
