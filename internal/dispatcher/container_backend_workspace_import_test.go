package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins what boid asks the ENGINE for when it migrates a workspace
// home (PR8 of docs/plans/workspace-home-volume-persistence.md, 論点 f). The
// daemon-side sequencing (lock, marker, the two re-init legs) is pinned
// separately, against a real tar, in workspace_home_import_test.go — neither
// file's coverage implies the other's.

func testWorkspaceImportRequest(tarBody string) WorkspaceHomeImportRequest {
	return WorkspaceHomeImportRequest{
		Slug:       "myws",
		HomeSource: dockerres.WorkspaceHomeVolumeName(testWorkspaceInitInstallID, "myws"),
		HomeTarget: "/home/boid",
		HomeID:     "deadbeef",
		Tar:        strings.NewReader(tarBody),
	}
}

// TestContainerBackend_ImplementsWorkspaceHomeImporter is the type-level half
// of the wiring: Runner.ImportWorkspaceHome discovers the capability by
// asserting on Runner.Backend, so a containerBackend that stopped satisfying
// this interface would compile fine and fail every migration at run time with
// "the configured sandbox backend cannot import a workspace home".
func TestContainerBackend_ImplementsWorkspaceHomeImporter(t *testing.T) {
	var be backend.SandboxBackend = NewContainerBackend(&fakeDockerAPI{}, ContainerBackendOptions{})
	if _, ok := be.(WorkspaceHomeImporter); !ok {
		t.Fatal("the container backend does not satisfy WorkspaceHomeImporter")
	}
}

// TestContainerBackend_RemoveWorkspaceHomeVolume_Unforced pins D4's first
// requirement, and the reason it is unforced.
func TestContainerBackend_RemoveWorkspaceHomeVolume_Unforced(t *testing.T) {
	api := &fakeDockerAPI{}
	var gotName string
	var gotOpts client.VolumeRemoveOptions
	api.VolumeRemoveFunc = func(_ context.Context, name string, opts client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
		gotName, gotOpts = name, opts
		return client.VolumeRemoveResult{}, nil
	}
	b := newWorkspaceInitBackend(api)

	name := dockerres.WorkspaceHomeVolumeName(testWorkspaceInitInstallID, "myws")
	existed, err := b.RemoveWorkspaceHomeVolume(context.Background(), name)
	if err != nil {
		t.Fatalf("RemoveWorkspaceHomeVolume: %v", err)
	}
	if !existed {
		t.Error("existed = false after a successful removal: the CLI reports \"replaced\" vs \"created\" from this")
	}
	if gotName != name {
		t.Errorf("removed %q, want %q", gotName, name)
	}
	if gotOpts.Force {
		t.Error("Force is set: it makes a volume that was never there return 204 like a real deletion, and the " +
			"migration reports 'a workspace never dispatched into' as a distinct, expected state")
	}
}

// TestContainerBackend_RemoveWorkspaceHomeVolume_MissingIsNotAnError is the
// fresh-install / never-dispatched-into case: there is simply nothing to
// replace.
func TestContainerBackend_RemoveWorkspaceHomeVolume_MissingIsNotAnError(t *testing.T) {
	api := &fakeDockerAPI{VolumeRemoveFunc: func(context.Context, string, client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
		return client.VolumeRemoveResult{}, errdefs.ErrNotFound.WithMessage("no such volume")
	}}
	b := newWorkspaceInitBackend(api)

	existed, err := b.RemoveWorkspaceHomeVolume(context.Background(), dockerres.WorkspaceHomeVolumeName(testWorkspaceInitInstallID, "myws"))
	if err != nil {
		t.Errorf("RemoveWorkspaceHomeVolume on a missing volume = %v, want nil", err)
	}
	if existed {
		t.Error("existed = true for a volume the engine 404'd on")
	}
}

// TestContainerBackend_RemoveWorkspaceHomeVolume_ConflictIsInUse pins the
// translation D4 depends on: the engine's 409 has to arrive at the API layer as
// something it can turn into a 409 of its own, and at the CLI as something it
// can explain, without either of them matching on engine prose.
func TestContainerBackend_RemoveWorkspaceHomeVolume_ConflictIsInUse(t *testing.T) {
	api := &fakeDockerAPI{VolumeRemoveFunc: func(context.Context, string, client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
		return client.VolumeRemoveResult{}, errdefs.ErrConflict.WithMessage("volume is being used by the following container(s): abc123")
	}}
	b := newWorkspaceInitBackend(api)

	_, err := b.RemoveWorkspaceHomeVolume(context.Background(), dockerres.WorkspaceHomeVolumeName(testWorkspaceInitInstallID, "myws"))
	if !errors.Is(err, ErrWorkspaceHomeInUse) {
		t.Fatalf("error = %v, want it to wrap ErrWorkspaceHomeInUse", err)
	}
	if !strings.Contains(err.Error(), "abc123") {
		t.Errorf("error %q drops the engine's own message, which is what names the container in the way", err)
	}
}

// TestContainerBackend_ImportWorkspaceHome_CreateOptions pins the whole create
// call in one place, for the reason its RunWorkspaceInit counterpart does: most
// fields are load-bearing for a different reason and a partial assertion leaves
// the rest free to regress silently.
func TestContainerBackend_ImportWorkspaceHome_CreateOptions(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	b := newWorkspaceInitBackend(api)
	req := testWorkspaceImportRequest("tar bytes")

	if err := b.ImportWorkspaceHome(context.Background(), req); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate called %d times, want 1", len(api.createCalls))
	}
	opts := api.createCalls[0]

	// The name is shared with the init container ON PURPOSE: it is what makes a
	// migration and an init mutually exclusive on the engine, across processes,
	// where the daemon's flock cannot reach.
	if want := dockerres.WorkspaceInitContainerName(testWorkspaceInitInstallID, "myws"); opts.Name != want {
		t.Errorf("container name = %q, want %q (shared with the init container so the two cannot run at once)", opts.Name, want)
	}
	if got := opts.Config.Labels[dockerres.LabelWorkspaceInit]; got != "myws" {
		t.Errorf("%s = %q, want %q — without it the startup sweep cannot collect a leaked import container, and "+
			"clearWorkspaceInitContainer refuses to touch the name", dockerres.LabelWorkspaceInit, got, "myws")
	}
	if got := opts.Config.Labels[dockerres.LabelWorkspaceInitInstallID]; got != testWorkspaceInitInstallID {
		t.Errorf("%s = %q, want %q", dockerres.LabelWorkspaceInitInstallID, got, testWorkspaceInitInstallID)
	}

	// The extraction command line. Asserted against the shared builder rather
	// than a literal, so this test cannot pass a wrong flag set that
	// workspaceHomeImportArgv also emits — the flags themselves are pinned by
	// TestWorkspaceHomeImportArgv below and, behaviourally, by
	// TestRunner_ImportWorkspaceHome_PreservesModes.
	wantArgv := workspaceHomeImportArgv("/home/boid")
	if strings.Join(opts.Config.Entrypoint, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("Entrypoint = %v, want %v", opts.Config.Entrypoint, wantArgv)
	}
	if opts.Config.Cmd == nil {
		t.Error("Cmd is nil, so the engine appends the image's own CMD to the tar command line as member operands: " +
			"tar would then extract only the members those words happen to name")
	}
	if len(opts.Config.Cmd) != 0 {
		t.Errorf("Cmd = %v, want empty", opts.Config.Cmd)
	}
	if len(opts.Config.Env) != 0 {
		t.Errorf("Env = %v, want none: an archive extraction needs nothing from the environment", opts.Config.Env)
	}
	if want := fmt.Sprintf("%d:%d", 4242, 4243); opts.Config.User != want {
		t.Errorf("User = %q, want %q (the uid a JOB container runs as — the whole point of --no-same-owner)", opts.Config.User, want)
	}
	if !opts.Config.OpenStdin || !opts.Config.AttachStdin || !opts.Config.StdinOnce {
		t.Error("stdin is not open/attached/one-shot; the archive arrives on stdin and tar needs the EOF to finish")
	}
	if opts.Config.Tty {
		t.Error("Tty is set: the attach stream would lose docker's framing and tar's stderr would be mixed into the payload path")
	}

	if opts.HostConfig.NetworkMode != "none" {
		t.Errorf("NetworkMode = %q, want \"none\": unlike an init.sh (which downloads a toolchain) this run reads a pipe "+
			"and writes a filesystem", opts.HostConfig.NetworkMode)
	}
	if len(opts.HostConfig.Mounts) != 1 {
		t.Fatalf("Mounts = %+v, want exactly the home volume", opts.HostConfig.Mounts)
	}
	m := opts.HostConfig.Mounts[0]
	if m.Type != mount.TypeVolume || m.Source != req.HomeSource || m.Target != req.HomeTarget {
		t.Errorf("mount = %+v, want a volume mount of %q at %q", m, req.HomeSource, req.HomeTarget)
	}
}

// TestContainerBackend_ImportWorkspaceHome_StreamsTheArchiveToStdin pins the
// payload path: the bytes the daemon was handed are the bytes tar reads.
func TestContainerBackend_ImportWorkspaceHome_StreamsTheArchiveToStdin(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	b := newWorkspaceInitBackend(api)

	payload := strings.Repeat("archive-", 5000)
	if err := b.ImportWorkspaceHome(context.Background(), testWorkspaceImportRequest(payload)); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	if got := string(joinBytes(api.lastAttachConn(t).writes)); got != payload {
		t.Errorf("stdin received %d bytes, want the %d-byte archive verbatim", len(got), len(payload))
	}
}

// TestContainerBackend_ImportWorkspaceHome_EnsuresTheVolumeAndChecksTheAnswer
// covers the trap RunWorkspaceInit's own §D4 note describes, which applies
// identically here: without an explicit VolumeCreate, ContainerCreate creates
// the volume implicitly and UNLABELLED, and no later dispatch can repair that.
func TestContainerBackend_ImportWorkspaceHome_EnsuresTheVolumeAndChecksTheAnswer(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	b := newWorkspaceInitBackend(api)
	req := testWorkspaceImportRequest("x")

	if err := b.ImportWorkspaceHome(context.Background(), req); err != nil {
		t.Fatalf("ImportWorkspaceHome: %v", err)
	}
	if len(api.volumeCreateCalls) != 1 {
		t.Fatalf("VolumeCreate called %d times, want 1", len(api.volumeCreateCalls))
	}
	if got := api.volumeCreateCalls[0].Labels[dockerres.LabelWorkspaceHomeID]; got != req.HomeID {
		t.Errorf("stamped identity %q, want %q", got, req.HomeID)
	}
}

// TestContainerBackend_ImportWorkspaceHome_RefusesAReplacedVolume is the other
// half of that check: a volume removed and re-created between
// Runner.ImportWorkspaceHome's own create and this one carries a different
// identity, and extracting into it would leave the daemon reporting a migration
// into a volume nobody prepared.
func TestContainerBackend_ImportWorkspaceHome_RefusesAReplacedVolume(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	req := testWorkspaceImportRequest("x")
	api.seedVolume(req.HomeSource, map[string]string{dockerres.LabelWorkspaceHomeID: "somebody-elses-incarnation"})
	b := newWorkspaceInitBackend(api)

	err := b.ImportWorkspaceHome(context.Background(), req)
	if err == nil {
		t.Fatal("ImportWorkspaceHome extracted into a volume that is not the one the daemon resolved")
	}
	if !strings.Contains(err.Error(), "somebody-elses-incarnation") {
		t.Errorf("error %q does not name the identity actually found", err)
	}
	if len(api.createCalls) != 0 {
		t.Error("a container was created despite the identity mismatch")
	}
}

// TestContainerBackend_ImportWorkspaceHome_NonZeroExitCarriesTarsOutput: the
// container and its logs are gone by the time anything renders this error, so
// the tail is the only description of what went wrong — and the daemon has
// already destroyed the old volume, which makes it the only description of what
// the operator has lost.
func TestContainerBackend_ImportWorkspaceHome_NonZeroExitCarriesTarsOutput(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(2)}
	api.ContainerAttachFunc = attachThenEOF(api, "tar: Unexpected EOF in archive\n")
	b := newWorkspaceInitBackend(api)

	err := b.ImportWorkspaceHome(context.Background(), testWorkspaceImportRequest("truncated"))
	if err == nil {
		t.Fatal("ImportWorkspaceHome returned nil for a tar that exited 2")
	}
	if !strings.Contains(err.Error(), "Unexpected EOF in archive") {
		t.Errorf("error %q does not carry tar's own output", err)
	}
	if !strings.Contains(err.Error(), "exited 2") {
		t.Errorf("error %q does not report the exit code", err)
	}
}

// TestContainerBackend_ImportWorkspaceHome_RejectsAPathAsTheHomeSource is the
// inverted guard workspaceHomeVolumeMount carries, exercised from this side: an
// absolute source would silently bind a host directory and the extraction would
// "succeed" into somewhere that is not the workspace's home.
func TestContainerBackend_ImportWorkspaceHome_RejectsAPathAsTheHomeSource(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	b := newWorkspaceInitBackend(api)
	req := testWorkspaceImportRequest("x")
	req.HomeSource = "/home/nosen/.local/share/boid/homes/myws"

	if err := b.ImportWorkspaceHome(context.Background(), req); err == nil {
		t.Fatal("an absolute host path was accepted as the home source")
	}
	if len(api.createCalls) != 0 {
		t.Error("a container was created for an absolute home source")
	}
}

// TestWorkspaceHomeImportArgv pins the flags themselves, with the reason each
// one is there. The behavioural proof that --same-permissions is load-bearing
// is TestRunner_ImportWorkspaceHome_PreservesModes, which runs a real tar; this
// is the statement of intent that makes an accidental removal read as a
// deliberate one.
func TestWorkspaceHomeImportArgv(t *testing.T) {
	argv := workspaceHomeImportArgv("/home/boid")
	joined := strings.Join(argv, " ")

	if argv[0] != "tar" {
		t.Errorf("argv[0] = %q, want tar", argv[0])
	}
	for _, want := range []string{
		"--extract",
		"--file -",               // stdin, where the daemon writes the archive
		"--directory /home/boid", // the volume's mount point; entry names are relative
		"--no-same-owner",        // D3: never carry the host's uids across the mapping
		"--same-permissions",     // D3: 0600 must stay 0600, and 0755 must stay 0755
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v is missing %q", argv, want)
		}
	}
	if strings.Contains(joined, "--same-owner") && !strings.Contains(joined, "--no-same-owner") {
		t.Error("--same-owner would need root and would re-introduce the host's uids")
	}
}

// TestContainerBackend_ImportWorkspaceHome_BoundsTheContainerCleanup is codex
// round 2's Major 3, and the failure it describes is the nastiest kind: a
// deadlock in the code that exists to make a failure recoverable.
//
// The extraction container's removal is deferred and, because the caller's
// context is typically already cancelled by then (the operator's ^C during the
// upload IS the commonest failure), it must not use that context. Dropping
// cancellation without adding a bound of its own leaves an engine call that can
// never return: with a socket that accepts and then goes silent, the defer
// blocks forever, Runner.ImportWorkspaceHome never gets its error back, its own
// bounded volume cleanup never runs, and — because the migration's in-flight
// registration is released by a defer of its own — every later dispatch of that
// workspace parks in beginDispatch waiting for a migration that has already
// stopped making progress.
//
// The engine is modelled as accepting the request and never answering, which is
// what a wedged socket looks like from the client side, and is a state no
// timeout on the engine's side would rescue.
func TestContainerBackend_ImportWorkspaceHome_BoundsTheContainerCleanup(t *testing.T) {
	restore := shrinkWorkspaceHomeContainerCleanupTimeout(t, 100*time.Millisecond)
	defer restore()

	removeCtx := make(chan context.Context, 4)
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerRemoveFunc = func(ctx context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
		removeCtx <- ctx
		<-ctx.Done() // an engine that accepted the request and never answers
		return client.ContainerRemoveResult{}, ctx.Err()
	}
	b := newWorkspaceInitBackend(api)

	// Already cancelled, exactly as it is after the CLI drops the connection:
	// the removal must neither inherit that cancellation (it would never run)
	// nor inherit the absence of a deadline (it would never return).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- b.ImportWorkspaceHome(ctx, testWorkspaceImportRequest("tar bytes")) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ImportWorkspaceHome never returned: the deferred ContainerRemove is issued on a context with no " +
			"deadline, so an engine that accepts the request and never answers blocks the migration forever — the " +
			"caller never sees the extraction error, the 30s-bounded volume cleanup never runs, and the workspace's " +
			"in-flight registration is never released")
	}

	select {
	case got := <-removeCtx:
		if _, ok := got.Deadline(); !ok {
			t.Error("the cleanup removal was issued on a context with no deadline")
		}
		if got.Err() != nil && errors.Is(got.Err(), context.Canceled) {
			t.Error("the cleanup removal inherited the caller's cancellation, so it cannot run in the case it exists for")
		}
	default:
		t.Fatal("ContainerRemove was never called: the extraction container is leaked")
	}
}

// TestContainerBackend_RunWorkspaceInit_BoundsTheContainerCleanup is the same
// defect on the same machinery's other consumer (PR5's init container), found by
// the grep codex round 2 asked for. It is worse here than for the migration:
// this defer runs while resolveWorkspaceHome holds the workspace's init flock,
// so a wedged engine socket hangs every future dispatch of that workspace on a
// syscall no context can interrupt, not just the migration.
func TestContainerBackend_RunWorkspaceInit_BoundsTheContainerCleanup(t *testing.T) {
	restore := shrinkWorkspaceHomeContainerCleanupTimeout(t, 100*time.Millisecond)
	defer restore()

	removeCtx := make(chan context.Context, 4)
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerRemoveFunc = func(ctx context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
		removeCtx <- ctx
		<-ctx.Done()
		return client.ContainerRemoveResult{}, ctx.Err()
	}
	b := newWorkspaceInitBackend(api)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- b.RunWorkspaceInit(ctx, testWorkspaceInitRequest()) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunWorkspaceInit never returned: the deferred ContainerRemove has no deadline, so a wedged engine " +
			"socket pins the caller — which holds this workspace's init flock — indefinitely")
	}

	select {
	case got := <-removeCtx:
		if _, ok := got.Deadline(); !ok {
			t.Error("the cleanup removal was issued on a context with no deadline")
		}
	default:
		t.Fatal("ContainerRemove was never called: the init container is leaked")
	}
}

// shrinkWorkspaceHomeContainerCleanupTimeout narrows the bound the two tests
// above wait on, so "the call returns" is observable in milliseconds instead of
// the 30 seconds the production value is set to for the reason its own doc
// comment gives.
//
// A package-level var rather than a parameter because the value is a policy of
// this file, not of a call site; the same shape newWorkspaceHomeStoreFn in
// internal/server uses. No test in this package calls t.Parallel(), so a
// process-wide swap is safe here (the same standing assumption
// setUmaskForTest relies on).
func shrinkWorkspaceHomeContainerCleanupTimeout(t *testing.T, d time.Duration) (restore func()) {
	t.Helper()
	prev := workspaceHomeContainerCleanupTimeout.Set(d)
	restore = func() { workspaceHomeContainerCleanupTimeout.Set(prev) }
	t.Cleanup(restore)
	return restore
}
