package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins what boid asks the ENGINE for when it prepares a workspace
// home (PR5 of docs/plans/workspace-home-volume-persistence.md, 論点 c): the
// container's name, labels, user, userns mode, network, mount and lifecycle.
// The wrapper script it feeds that container is pinned separately, against a
// real bash, in workspace_init_test.go — neither file's coverage implies the
// other's.

const testWorkspaceInitInstallID = "0123456789abcdef"

// newWorkspaceInitBackend builds a containerBackend over api with the uid/gid
// and install id these tests assert against.
func newWorkspaceInitBackend(api dockerAPI) *containerBackend {
	uid, gid := 4242, 4243
	return NewContainerBackend(api, ContainerBackendOptions{
		InstallID: testWorkspaceInitInstallID,
		UID:       &uid,
		GID:       &gid,
	}).(*containerBackend)
}

func testWorkspaceInitRequest() WorkspaceInitRequest {
	return WorkspaceInitRequest{
		Slug: "myws",
		// A VOLUME NAME as of PR6, not a path — see workspaceInitHomeMount.
		HomeSource: dockerres.WorkspaceHomeVolumeName(testWorkspaceInitInstallID, "myws"),
		HomeTarget: "/home/boid",
		Env: map[string]string{
			"HOME":                "/home/boid",
			"BOID_WORKSPACE_SLUG": "myws",
			"BOID_WORKSPACE_HOME": "/home/boid",
		},
		Script:       "# wrapper\nexit 0\n",
		SkeletonDirs: []string{".claude"},
		HomeID:       "deadbeef",
	}
}

// attachThenEOF makes ContainerAttach hand back a connection that emits the
// given output on docker's multiplexed stdout stream and then closes, so the
// implementation's drain finishes immediately instead of waiting out its grace
// period.
func attachThenEOF(api *fakeDockerAPI, output string) func(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	return func(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
		conn := newFakeAttachConn()
		api.recordAttachConn(conn)
		go func() {
			if output != "" {
				conn.feedFrame(1, []byte(output))
			}
			_ = conn.Close()
		}()
		return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(conn, "")}, nil
	}
}

// waitResponding hands ContainerWait one already-resolved WaitResponse.
//
// It takes the whole response rather than an exit code because a WaitResponse
// carries TWO independent facts: StatusCode, the container process's exit
// status, and Error, which the engine sets when the wait itself did not produce
// one. A helper that could only express the first is why nothing exercised the
// second — see
// TestContainerBackend_RunWorkspaceInit_EngineErrorInTheWaitResponseFailsTheRun.
func waitResponding(res container.WaitResponse) func(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	return func(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
		ch := make(chan container.WaitResponse, 1)
		ch <- res
		return client.ContainerWaitResult{Result: ch, Error: make(chan error, 1)}
	}
}

func exitWith(code int) func(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	return waitResponding(container.WaitResponse{StatusCode: int64(code)})
}

// TestContainerBackend_ImplementsWorkspaceInitExecutor is the type-level half
// of the wiring: resolveWorkspaceHome discovers the capability by asserting on
// Runner.Backend (§D10), so a containerBackend that stopped satisfying the
// interface would not fail to compile anywhere — it would fail every dispatch
// at runtime with "the configured sandbox backend cannot run a workspace home
// init container".
func TestContainerBackend_ImplementsWorkspaceInitExecutor(t *testing.T) {
	var be backend.SandboxBackend = NewContainerBackend(&fakeDockerAPI{}, ContainerBackendOptions{})
	if _, ok := be.(WorkspaceInitExecutor); !ok {
		t.Fatal("the container backend does not satisfy WorkspaceInitExecutor; every workspace home init would fail at runtime")
	}
}

// TestContainerBackend_RunWorkspaceInit_CreateOptions pins the whole create
// call in one place, because most of the fields are load-bearing for a
// different reason and a partial assertion would leave the others free to
// regress silently.
func TestContainerBackend_RunWorkspaceInit_CreateOptions(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	b := newWorkspaceInitBackend(api)
	req := testWorkspaceInitRequest()

	if err := b.RunWorkspaceInit(context.Background(), req); err != nil {
		t.Fatalf("RunWorkspaceInit: %v", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate called %d times, want 1", len(api.createCalls))
	}
	got := api.createCalls[0]

	// §D6: a deterministic name is the cross-process mutual exclusion, so it
	// has to be exactly the name any other process would compute.
	wantName := dockerres.WorkspaceInitContainerName(testWorkspaceInitInstallID, req.Slug)
	if got.Name != wantName {
		t.Errorf("container name = %q, want %q", got.Name, wantName)
	}

	// §D5: the two dedicated labels and nothing else.
	wantLabels := map[string]string{
		dockerres.LabelWorkspaceInit:          req.Slug,
		dockerres.LabelWorkspaceInitInstallID: testWorkspaceInitInstallID,
	}
	if len(got.Config.Labels) != len(wantLabels) {
		t.Errorf("labels = %v, want exactly %v", got.Config.Labels, wantLabels)
	}
	for k, v := range wantLabels {
		if got.Config.Labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, got.Config.Labels[k], v)
		}
	}
	for _, forbidden := range []string{labelJobID, labelInstallID, labelWorkspace} {
		if _, present := got.Config.Labels[forbidden]; present {
			t.Errorf("init container carries %q, which is an enumeration filter: ReapOrphans/reap.Run would force-remove it mid-install and (for boid.job_id) fold its value into ReapReport's job accounting", forbidden)
		}
	}

	// §D8: the image's ENTRYPOINT is `boid runner-container`, which is not
	// what this container runs. Launch has never set Entrypoint, so leaving it
	// unset here would start a job runner against a spec that does not exist.
	if len(got.Config.Entrypoint) != 2 || got.Config.Entrypoint[0] != "/bin/bash" || got.Config.Entrypoint[1] != "-s" {
		t.Errorf("entrypoint = %v, want [/bin/bash -s]", got.Config.Entrypoint)
	}
	// Cmd has to be a NON-NIL empty slice, and the distinction is the whole
	// point: the docker API omits a nil Cmd from the create body, and the
	// engine then falls back to the image's own CMD — which for
	// build/container/Dockerfile is a runner invocation. `len(Cmd) == 0` is
	// true for both, so asserting only on the length would pin nothing and
	// let a `Cmd: nil` regression through.
	if got.Config.Cmd == nil {
		t.Error("cmd is nil; a nil Cmd is omitted from the create request and the engine applies the image's own CMD")
	} else if len(got.Config.Cmd) != 0 {
		t.Errorf("cmd = %v, want an empty (but non-nil) slice", got.Config.Cmd)
	}
	if got.Config.Image != defaultContainerImage {
		t.Errorf("image = %q, want the backend default %q", got.Config.Image, defaultContainerImage)
	}

	// §D7: the same uid the job containers get, never a literal.
	if want := "4242:4243"; got.Config.User != want {
		t.Errorf("user = %q, want %q (the daemon's own uid/gid, as resolved by NewContainerBackend)", got.Config.User, want)
	}

	// The wrapper arrives on stdin, so stdin has to be open and attached.
	if !got.Config.OpenStdin || !got.Config.AttachStdin {
		t.Errorf("OpenStdin=%v AttachStdin=%v, want both true — the wrapper is delivered on stdin", got.Config.OpenStdin, got.Config.AttachStdin)
	}
	if !got.Config.AttachStdout || !got.Config.AttachStderr {
		t.Error("stdout/stderr must be attached; they are the only record of what the init printed")
	}
	if got.Config.Tty {
		t.Error("Tty must be false: there is no terminal, and a TTY would also merge the streams without docker's framing")
	}

	if got.Config.WorkingDir != req.HomeTarget {
		t.Errorf("workdir = %q, want the mounted home %q", got.Config.WorkingDir, req.HomeTarget)
	}
	wantEnv := envSlice(req.Env)
	if strings.Join(got.Config.Env, "\n") != strings.Join(wantEnv, "\n") {
		t.Errorf("env = %v, want %v", got.Config.Env, wantEnv)
	}

	// The mount: the workspace home VOLUME, read-write, at the same path a job
	// sees its $HOME at (PR6 — a bind of a host path here would prepare
	// something other than the home the job will actually get).
	if len(got.HostConfig.Mounts) != 1 {
		t.Fatalf("mounts = %+v, want exactly the workspace home", got.HostConfig.Mounts)
	}
	m := got.HostConfig.Mounts[0]
	if m.Type != mount.TypeVolume || m.Source != req.HomeSource || m.Target != req.HomeTarget || m.ReadOnly {
		t.Errorf("home mount = %+v, want a read-write VOLUME mount %s -> %s", m, req.HomeSource, req.HomeTarget)
	}

	// §D4 of 論点 a: the init container does not go through Launch, so nothing
	// else would create the volume with its labels — the engine would
	// auto-create it unlabelled when the container starts, and the identity
	// resolveWorkspaceHome compares its marker against would be permanently
	// absent.
	if len(api.volumeCreateCalls) != 1 {
		t.Fatalf("VolumeCreate calls = %d, want 1 (the init container must create the home volume itself)", len(api.volumeCreateCalls))
	}
	vc := api.volumeCreateCalls[0]
	if vc.Name != req.HomeSource {
		t.Errorf("VolumeCreate Name = %q, want %q", vc.Name, req.HomeSource)
	}
	if got, want := vc.Labels[dockerres.LabelWorkspaceHomeID], req.HomeID; got != want {
		t.Errorf("VolumeCreate Labels[%q] = %q, want the identity the daemon resolved (%q)",
			dockerres.LabelWorkspaceHomeID, got, want)
	}
	if got, want := vc.Labels[dockerres.LabelWorkspaceHome], req.Slug; got != want {
		t.Errorf("VolumeCreate Labels[%q] = %q, want %q", dockerres.LabelWorkspaceHome, got, want)
	}
	for _, forbidden := range []string{labelJobID, labelInstallID, labelWorkspace} {
		if _, present := vc.Labels[forbidden]; present {
			t.Errorf("the home volume carries %q, which is a reap enumeration filter (論点 a)", forbidden)
		}
	}

	// §D4: the engine's default bridge. Neither field is set — the job
	// containers' per-workspace network is `Internal: true` with egress only
	// through an allowlist proxy, under which a toolchain installer (~1.5GB of
	// go/node/volta/claude/codex/opencode) has essentially no reachable
	// mirrors.
	if got.HostConfig.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want unset (§D4: the engine's default bridge)", got.HostConfig.NetworkMode)
	}
	if got.NetworkingConfig != nil {
		t.Errorf("NetworkingConfig = %+v, want nil (§D4)", got.NetworkingConfig)
	}
	if len(api.networkCreateNames) != 0 {
		t.Errorf("an init run created networks %v; it must not touch the workspace network at all", api.networkCreateNames)
	}
}

// TestContainerBackend_RunWorkspaceInit_FeedsTheWrapperOnStdin pins §D2's
// delivery route. A file the daemon writes is not a path the sibling engine
// can mount (build/container/compose.yml's KNOWN GAP), so the bytes go over
// the attach connection — and the write side has to be half-closed afterwards
// or `bash -s` waits forever for more script.
func TestContainerBackend_RunWorkspaceInit_FeedsTheWrapperOnStdin(t *testing.T) {
	var conn *fakeAttachConn
	api := &fakeDockerAPI{
		ContainerAttachFunc: func(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
			conn = newFakeAttachConn()
			go func() { _ = conn.Close() }()
			return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(conn, "")}, nil
		},
		ContainerWaitFunc: exitWith(0),
	}
	b := newWorkspaceInitBackend(api)
	req := testWorkspaceInitRequest()

	if err := b.RunWorkspaceInit(context.Background(), req); err != nil {
		t.Fatalf("RunWorkspaceInit: %v", err)
	}
	if conn == nil {
		t.Fatal("no attach connection was established")
	}
	conn.mu.Lock()
	written := string(joinBytes(conn.writes))
	conn.mu.Unlock()
	if written != req.Script {
		t.Errorf("wrote %q to the container's stdin, want the wrapper %q", written, req.Script)
	}
	if conn.closeWrites == 0 {
		t.Error("the stdin half of the attach connection was never closed; `bash -s` would wait for more script forever")
	}
}

func joinBytes(chunks [][]byte) []byte {
	var out []byte
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

// captureLogs redirects the default slog logger into a buffer for the length
// of the test, at the given level.
func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// runInitPrinting runs a successful init whose container printed out.
func runInitPrinting(t *testing.T, out string) {
	t.Helper()
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, out)
	b := newWorkspaceInitBackend(api)
	if err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest()); err != nil {
		t.Fatalf("RunWorkspaceInit: %v", err)
	}
}

// TestContainerBackend_RunWorkspaceInit_SuccessKeepsTheOutput pins that a
// SUCCESSFUL init leaves a record of what the script printed.
//
// Only the failure path used to carry the output (workspaceInitFailure);
// exit 0 logged a byte COUNT and dropped the bytes. init.sh is the
// operator's own script — it installs toolchains, and the way it fails
// operationally is by "succeeding" while leaving something subtly wrong.
// That is exactly what happened on 2026-07-28: a run that exited 0 left a
// workspace whose volta shims were all dangling, and with the output gone
// there was nothing to read, so the diagnosis went the long way round
// through an overlay-mounted replay of the volume.
func TestContainerBackend_RunWorkspaceInit_SuccessKeepsTheOutput(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	const said = "volta: linking shims for node@24"

	runInitPrinting(t, said+"\nboid-init: stage=done exit=0\n")

	if !strings.Contains(buf.String(), said) {
		t.Errorf("a successful init discarded what its script printed; log was:\n%s", buf.String())
	}
}

// TestContainerBackend_RunWorkspaceInit_SuccessTailSurvivesAtInfoLevel pins
// that the record above does NOT depend on debug logging being on. A
// workspace init takes minutes (8–9 measured for a real toolchain install),
// so "reproduce it with -v" is not a diagnosis path an operator actually
// takes — the tail has to already be there at the default level.
func TestContainerBackend_RunWorkspaceInit_SuccessTailSurvivesAtInfoLevel(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	const lastThingSaid = "volta: linking shims for node@24"

	runInitPrinting(t, "earlier progress\n"+lastThingSaid+"\n")

	if !strings.Contains(buf.String(), lastThingSaid) {
		t.Errorf("nothing of the init output survives at info level; log was:\n%s", buf.String())
	}
}

// TestContainerBackend_RunWorkspaceInit_SuccessTailIsBounded is the other
// side of that trade: the default-level record must stay a TAIL. An init
// that prints megabytes of progress (npm, cargo, apt all do) would otherwise
// push that volume through the daemon log on every workspace's first
// dispatch.
func TestContainerBackend_RunWorkspaceInit_SuccessTailIsBounded(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	noisy := strings.Repeat("progress line that says very little\n", 40_000)

	runInitPrinting(t, noisy+"boid-init: stage=done exit=0\n")

	if got := buf.Len(); got > 8*workspaceInitSuccessTailLimit {
		t.Errorf("info-level log is %d bytes for a %d-byte init; the tail bound is not being applied", got, len(noisy))
	}
	if !strings.Contains(buf.String(), "stage=done exit=0") {
		t.Error("the bounded tail dropped the END of the output; it must keep the end, which is where a failure explains itself")
	}
}

// TestContainerBackend_RunWorkspaceInit_TruncatedTailSaysSo pins that a
// tail cut out of a longer run announces the cut. The bound is the whole
// reason this is safe to log unconditionally, so a reader has to be able to
// tell "this is everything the script printed" from "this is the last few
// lines of it" — otherwise the record invites the wrong conclusion about
// output that is simply not shown.
func TestContainerBackend_RunWorkspaceInit_TruncatedTailSaysSo(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	filler := strings.Repeat("...installing a package nobody will read about\n", 500)
	if len(filler) <= workspaceInitSuccessTailLimit {
		t.Fatalf("test is not exercising truncation: %d bytes of filler fits in a %d-byte tail",
			len(filler), workspaceInitSuccessTailLimit)
	}

	runInitPrinting(t, filler+"boid-init: stage=done exit=0\n")

	if !strings.Contains(buf.String(), workspaceInitTruncationPrefix) {
		t.Errorf("a truncated tail was logged with no notice that anything was cut; log was:\n%s", buf.String())
	}
}

// TestContainerBackend_RunWorkspaceInit_SuccessDebugCarriesFullOutput pins the
// PR #852 follow-up (doc comment on workspaceInitSuccessTailLimit): once
// log.level: debug is on (internal/config.LogConfig, internal/daemon.
// ApplyLogLevel — PR #858), a successful init logs the FULL retained window
// (output.String()), not just the workspaceInitSuccessTailLimit-byte tail the
// INFO line carries. The marker here sits well before the last
// workspaceInitSuccessTailLimit bytes of the run's output, so it can only
// reach the log through the full dump.
func TestContainerBackend_RunWorkspaceInit_SuccessDebugCarriesFullOutput(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	const earlyMarker = "FULL-OUTPUT-MARKER-EARLY"
	// Padding after the marker has to clear workspaceInitSuccessTailLimit so
	// the marker cannot be explained by the (unconditional) INFO tail alone.
	trailing := strings.Repeat("x", workspaceInitSuccessTailLimit+3000)

	runInitPrinting(t, earlyMarker+"\n"+trailing+"\nboid-init: stage=done exit=0\n")

	if !strings.Contains(buf.String(), earlyMarker) {
		t.Errorf("full output was not logged at debug level; log was missing %q:\n%s", earlyMarker, buf.String())
	}
}

// TestContainerBackend_RunWorkspaceInit_SuccessDebugOffOmitsFullOutput is the
// other side of that: with log.level left at its default (no DEBUG output),
// the full dump must not appear at all — only the bounded INFO tail. Run
// against the SAME output as the test above so a regression that logs the
// full dump unconditionally (ignoring the level) is caught here rather than
// only being a "nice to have" in the debug test.
func TestContainerBackend_RunWorkspaceInit_SuccessDebugOffOmitsFullOutput(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	const earlyMarker = "FULL-OUTPUT-MARKER-EARLY"
	trailing := strings.Repeat("x", workspaceInitSuccessTailLimit+3000)

	runInitPrinting(t, earlyMarker+"\n"+trailing+"\nboid-init: stage=done exit=0\n")

	if strings.Contains(buf.String(), earlyMarker) {
		t.Errorf("the full output leaked into the log with debug OFF; log was:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "level=DEBUG") {
		t.Errorf("a DEBUG-level record was emitted even though the handler's threshold is INFO; log was:\n%s", buf.String())
	}
}

// TestContainerBackend_RunWorkspaceInit_InfoTailPresentRegardlessOfDebugLevel
// is the regression guard the task calls out explicitly: the unconditional
// INFO output_tail line — the whole point of PR #852 — must survive adding
// the debug full-dump line untouched, whether or not log.level: debug is on.
// A change that moved the tail's log call under a debug-only branch, or
// demoted "workspace home init completed" from Info to Debug outright, would
// pass the debug-level sub-test but fail the info-level one.
func TestContainerBackend_RunWorkspaceInit_InfoTailPresentRegardlessOfDebugLevel(t *testing.T) {
	const said = "volta: linking shims for node@24"
	script := "earlier progress\n" + said + "\n"

	for _, lvl := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(lvl.String(), func(t *testing.T) {
			buf := captureLogs(t, lvl)
			runInitPrinting(t, script)

			if !strings.Contains(buf.String(), "output_tail=") {
				t.Errorf("no output_tail key at handler level %s; log was:\n%s", lvl, buf.String())
			}
			if !strings.Contains(buf.String(), said) {
				t.Errorf("output_tail did not carry the run's own output at handler level %s; log was:\n%s", lvl, buf.String())
			}
		})
	}
}

// TestContainerBackend_RunWorkspaceInit_SuccessDebugFullOutputIsRetentionBounded
// pins that the debug full dump does not open a new unbounded memory/log
// path: it is exactly output.String(), which workspaceInitOutput already
// caps at workspaceInitOutputLimit (see that type's trim). Feeds far more
// than the retention limit, then checks two things: the front of the stream
// really was dropped (proving the bound is doing something, not merely
// "happens to fit"), and the debug line still surfaces content from deep
// inside the retained window rather than only the INFO tail.
func TestContainerBackend_RunWorkspaceInit_SuccessDebugFullOutputIsRetentionBounded(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	const droppedMarker = "SHOULD-BE-DROPPED-FRONT-MARKER"
	const lateMarker = "FULL-OUTPUT-MARKER-LATE"
	filler := strings.Repeat("z", 3*workspaceInitOutputLimit)
	// Keeps lateMarker inside the retained (last workspaceInitOutputLimit
	// bytes) window while still clearing workspaceInitSuccessTailLimit, so a
	// pass here cannot be explained by the INFO tail alone.
	trailing := strings.Repeat("y", workspaceInitSuccessTailLimit+3000)

	runInitPrinting(t, droppedMarker+"\n"+filler+lateMarker+"\n"+trailing)

	if strings.Contains(buf.String(), droppedMarker) {
		t.Fatalf("test is not exercising retention trimming: the front-of-stream marker survived")
	}
	if !strings.Contains(buf.String(), lateMarker) {
		t.Errorf("the retained window did not reach the debug log; log was missing %q", lateMarker)
	}
	if got, bound := buf.Len(), 2*workspaceInitOutputLimit; got > bound {
		t.Errorf("captured log is %d bytes, which exceeds %d (2x workspaceInitOutputLimit=%d); "+
			"the debug full dump is not respecting the retention bound", got, bound, workspaceInitOutputLimit)
	}
}

// captureDefaultLoggerOutput redirects the standard "log" package's output —
// exactly what slog's package-private DEFAULT handler writes through, as
// long as nothing calls slog.SetDefault (see internal/daemon.ApplyLogLevel's
// doc comment for why that invariant matters in production) — into an
// in-memory buffer for the duration of the test, and sets slog's bridge
// level the same way internal/daemon.ApplyLogLevel does (slog.
// SetLogLoggerLevel, never a custom Handler).
//
// captureLogs above installs a slog.NewTextHandler via slog.SetDefault
// instead, which is enough to pin log CONTENT (it's what every other test in
// this file uses) but is a different code path from the one production
// actually runs through. Duplicated here rather than reusing
// internal/daemon/log_level_test.go's captureStdLog because that package
// depends on internal/config, and the dependency here would have to run the
// other way.
func captureDefaultLoggerOutput(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(log.LstdFlags)
	slog.SetLogLoggerLevel(level)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		slog.SetLogLoggerLevel(slog.LevelInfo)
	})
	return &buf
}

// TestContainerBackend_RunWorkspaceInit_SuccessDebugOffSkipsMaterializingFullOutput
// pins NB-1 of PR #861's independent (Claude/Opus) review: with debug OFF,
// a successful init must not pay the cost of materializing the full retained
// window at all, not merely "not log it".
//
// Go evaluates a function call's arguments before the call itself runs, so
// `slog.Debug(msg, "output", output.String())` executes output.String() —
// the whole trimmed buffer copied into a fresh string, up to
// workspaceInitOutputLimit bytes — EVERY time, whether or not slog.Debug's
// own internal Enabled() check then throws the record away. Passing output
// itself (a fmt.Stringer, via its pointer receiver String method) instead of
// output.String() lets slog defer that conversion to whichever
// Handler.Handle renders the record, and Handle is never invoked for a
// level the logger has disabled — so the cost only exists when something is
// actually listening at DEBUG.
//
// workspaceInitOutputStringCalls is the only way to observe this: nothing
// outside workspace_init.go ever gets a handle on the *workspaceInitOutput a
// RunWorkspaceInit call constructs, so the log CONTENT looks identical
// either way (the earlier tests above already pin that) and only a call
// counter can tell eager from lazy apart.
func TestContainerBackend_RunWorkspaceInit_SuccessDebugOffSkipsMaterializingFullOutput(t *testing.T) {
	for _, tc := range []struct {
		name      string
		level     slog.Level
		wantCalls int64
	}{
		{"debug disabled (INFO threshold)", slog.LevelInfo, 0},
		{"debug enabled", slog.LevelDebug, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t, tc.level)
			workspaceInitOutputStringCalls.Store(0)

			runInitPrinting(t, "some output\n")

			if got := workspaceInitOutputStringCalls.Load(); got != tc.wantCalls {
				t.Errorf("workspaceInitOutput.String() called %d time(s) at handler level %s, want %d",
					got, tc.level, tc.wantCalls)
			}
		})
	}
}

// TestContainerBackend_RunWorkspaceInit_SuccessDebugOffSkipsMaterializingFullOutput_RealHandler
// re-runs the pin above against the handler PRODUCTION actually uses —
// slog's package-private default, bridged through the standard "log"
// package and gated by slog.SetLogLoggerLevel (internal/daemon.
// ApplyLogLevel), never a slog.NewTextHandler installed via SetDefault. The
// two are not guaranteed to defer a fmt.Stringer argument identically, so
// this confirms the property against the handler that ships rather than
// leaving it as an inference from the TextHandler-based test above.
func TestContainerBackend_RunWorkspaceInit_SuccessDebugOffSkipsMaterializingFullOutput_RealHandler(t *testing.T) {
	for _, tc := range []struct {
		name      string
		level     slog.Level
		wantCalls int64
	}{
		{"debug disabled (INFO threshold)", slog.LevelInfo, 0},
		{"debug enabled", slog.LevelDebug, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureDefaultLoggerOutput(t, tc.level)
			workspaceInitOutputStringCalls.Store(0)

			runInitPrinting(t, "some output\n")

			if got := workspaceInitOutputStringCalls.Load(); got != tc.wantCalls {
				t.Errorf("workspaceInitOutput.String() called %d time(s) at logger level %s (real default handler), want %d",
					got, tc.level, tc.wantCalls)
			}
		})
	}
}

// TestContainerBackend_RunWorkspaceInit_NonZeroExitCarriesStageAndOutput pins
// that a failure is reported with the container's own exit status and the
// output that explains it. Neither survives the call otherwise: the container
// is removed and its logs go with it.
func TestContainerBackend_RunWorkspaceInit_NonZeroExitCarriesStageAndOutput(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(7)}
	api.ContainerAttachFunc = attachThenEOF(api, "installer said no\nboid-init: stage=init.sh exit=7\n")
	b := newWorkspaceInitBackend(api)

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	if err == nil {
		t.Fatal("RunWorkspaceInit returned nil for a container that exited 7")
	}
	for _, want := range []string{`"myws"`, workspaceInitStageUserScript, "7", "installer said no"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

// TestContainerBackend_RunWorkspaceInit_EngineErrorInTheWaitResponseFailsTheRun
// pins that an engine that answers the wait with an ERROR does not produce a
// successful init.
//
// container.WaitResponse has two fields and only one of them is an exit status.
// When the engine cannot report one — the runtime failed to wait, the container
// never started, the shim died — it answers 200 with
// `{"StatusCode":0,"Error":{"Message":"…"}}`, and the SDK forwards that
// verbatim on the Result channel (moby/moby/client@v0.5.0 container_wait.go
// decodes the body and does not inspect Error). A caller reading only
// StatusCode therefore reads the ZERO VALUE of a field the engine never filled
// in and calls it "exited 0".
//
// The consequence is worse than one bad dispatch, which is why this is pinned
// rather than left to the exit-code path: resolveWorkspaceHome writes the
// completion marker and the home identity nonce on success, so a single
// fabricated success makes every LATER dispatch skip init entirely — against a
// home nothing ever prepared. There is no second chance to notice.
func TestContainerBackend_RunWorkspaceInit_EngineErrorInTheWaitResponseFailsTheRun(t *testing.T) {
	const engineMessage = "runtime wait failed"
	api := &fakeDockerAPI{ContainerWaitFunc: waitResponding(container.WaitResponse{
		// Exactly the shape that makes the omission invisible: the status
		// code is the success value, and the only evidence is in Error.
		StatusCode: 0,
		Error:      &container.WaitExitError{Message: engineMessage},
	})}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	b := newWorkspaceInitBackend(api)

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	if err == nil {
		t.Fatal("RunWorkspaceInit returned nil for a wait the engine answered with an error; the workspace home would be marked prepared and never initialized again")
	}
	msg := err.Error()
	if !strings.Contains(msg, engineMessage) {
		t.Errorf("the error does not carry the engine's own message %q, which is the only description of what went wrong:\n%v", engineMessage, err)
	}
	if !strings.Contains(msg, "wait response") {
		t.Errorf("the error does not say the failure came from the engine's WAIT RESPONSE; an operator cannot tell it from a script that exited non-zero:\n%v", err)
	}
	// The init script's exit status is not what failed here, and reporting it
	// as one sends the operator to init.sh for an engine fault.
	if strings.Contains(msg, "failed at stage") {
		t.Errorf("the error is rendered as a stage failure; the engine never reported an exit status for any stage to have produced:\n%v", err)
	}
}

// TestContainerBackend_RunWorkspaceInit_BoundedOutputKeepsTheTail runs the
// REAL capture path — attach, demux, bound, classify, render — against an
// init that printed more than the retention limit before failing.
//
// This is the shape of every interesting failure. An init.sh downloading a
// toolchain prints megabytes of progress, and the two things that diagnose it
// are both at the END: the installer's own last words, and the wrapper's
// `boid-init: stage=` marker, which is printed AFTER the user script exits.
// A capture that keeps the FIRST N bytes throws away both — the error then
// shows unrelated early progress output, and, having no marker to read, falls
// back to the exit-code table and blames a stage that never ran (here: exit 91
// would be reported as "prelude" when init.sh is what failed).
//
// The exit code is deliberately workspaceInitPreludeExitCode: it is the value
// that makes head-retention fail LOUDLY WRONG rather than merely unhelpfully.
func TestContainerBackend_RunWorkspaceInit_BoundedOutputKeepsTheTail(t *testing.T) {
	const (
		frameSize = 64 << 10
		frames    = workspaceInitOutputLimit/frameSize + 8 // comfortably past the cap
		lastLine  = "curl: (22) The requested URL returned error: 404"
	)
	marker := workspaceInitStageMarker + workspaceInitStageUserScript +
		" exit=" + strconv.Itoa(workspaceInitPreludeExitCode) + "\n"

	fed := make(chan struct{})
	api := &fakeDockerAPI{}
	api.ContainerAttachFunc = func(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
		conn := newFakeAttachConn()
		api.recordAttachConn(conn)
		go func() {
			// Many frames rather than one giant one: the bound has to hold
			// across an incremental stream, which is the only kind a real
			// attach produces.
			noise := bytes.Repeat([]byte("progress "), frameSize/9)
			for i := 0; i < frames; i++ {
				conn.feedFrame(1, noise)
			}
			conn.feedFrame(1, []byte(lastLine+"\n"))
			conn.feedFrame(2, []byte(marker))
			_ = conn.Close()
			close(fed)
		}()
		return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(conn, "")}, nil
	}
	// Resolve the wait only once the whole stream has been delivered, so the
	// assertion is about the bound and never about the drain grace period.
	api.ContainerWaitFunc = func(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
		res := make(chan container.WaitResponse, 1)
		go func() {
			<-fed
			res <- container.WaitResponse{StatusCode: int64(workspaceInitPreludeExitCode)}
		}()
		return client.ContainerWaitResult{Result: res, Error: make(chan error, 1)}
	}
	b := newWorkspaceInitBackend(api)

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	if err == nil {
		t.Fatalf("RunWorkspaceInit returned nil for a container that exited %d", workspaceInitPreludeExitCode)
	}
	msg := err.Error()

	if !strings.Contains(msg, lastLine) {
		t.Errorf("the error does not carry the LAST line the run printed; a head-retaining capture would have kept the first %d bytes instead:\n%s",
			workspaceInitOutputLimit, msg)
	}
	if want := `stage "` + workspaceInitStageUserScript + `"`; !strings.Contains(msg, want) {
		t.Errorf("the error does not report %s; the wrapper's marker is printed after the user script and is lost by a head-retaining capture:\n%s", want, msg)
	}
	if bad := `stage "` + workspaceInitStagePrelude + `"`; strings.Contains(msg, bad) {
		t.Errorf("the error reports %s: with the marker discarded, exit %d falls through to the code table and blames a stage that never ran:\n%s",
			bad, workspaceInitPreludeExitCode, msg)
	}
	// Dropping output is fine; dropping it silently is not — an operator
	// reading "output tail" has to know it is a tail of a tail.
	if !strings.Contains(msg, workspaceInitTruncationPrefix) {
		t.Errorf("the error does not say that output was omitted (%q):\n%s", workspaceInitTruncationPrefix, msg)
	}
	if len(msg) > workspaceInitOutputTail*2 {
		t.Errorf("the error is %d bytes; %d frames of progress output must not end up in it", len(msg), frames)
	}
}

// TestContainerBackend_RunWorkspaceInit_AlwaysRemovesTheContainer pins the
// throwaway half of "throwaway container". A leaked one would also hold the
// deterministic name, so the NEXT init for that workspace would collide with
// it and have to clean up after this run before it could start.
func TestContainerBackend_RunWorkspaceInit_AlwaysRemovesTheContainer(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{{"success", 0}, {"failure", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeDockerAPI{ContainerWaitFunc: exitWith(tc.code)}
			api.ContainerAttachFunc = attachThenEOF(api, "")
			b := newWorkspaceInitBackend(api)
			_ = b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())

			api.mu.Lock()
			defer api.mu.Unlock()
			if len(api.removeIDs) == 0 {
				t.Error("the init container was never removed")
			}
		})
	}
}

// TestContainerBackend_RunWorkspaceInit_NameConflictWithARunningContainer is
// §D6's core case, and the reason the name is deterministic at all: a flock
// dies with its process, an init container does not. After a daemon crash the
// old container may still be minutes into a 1.5GB download against the very
// home this run is about to prepare, so the run WAITS for it rather than
// racing it — Phase 4's 「同時 dispatch でも実行は 1 回」contract
// (docs/plans/home-workspace-volume.md:83) is what would otherwise break.
func TestContainerBackend_RunWorkspaceInit_NameConflictWithARunningContainer(t *testing.T) {
	var creates int
	waited := make(chan struct{})
	api := &fakeDockerAPI{}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		creates++
		if creates == 1 {
			return client.ContainerCreateResult{}, fakeConflictError{}
		}
		return client.ContainerCreateResult{ID: "fresh"}, nil
	}
	api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
		return incumbentInitContainerInspect("incumbent-id", true), nil
	}
	api.ContainerWaitFunc = func(_ context.Context, id string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
		res := make(chan container.WaitResponse, 1)
		if id == "incumbent-id" {
			close(waited)
		}
		res <- container.WaitResponse{StatusCode: 0}
		return client.ContainerWaitResult{Result: res, Error: make(chan error, 1)}
	}
	b := newWorkspaceInitBackend(api)

	if err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest()); err != nil {
		t.Fatalf("RunWorkspaceInit: %v", err)
	}
	select {
	case <-waited:
	default:
		t.Error("the run did not wait for the incumbent init container to finish before removing it")
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if creates != 2 {
		t.Errorf("ContainerCreate called %d times, want 2 (the conflict, then the retry)", creates)
	}
	// Removed by the ID the inspect reported, never by the name. See
	// TestContainerBackend_RunWorkspaceInit_NameConflictActsOnTheInspectedID
	// for why that distinction is the interesting one.
	var removedIncumbent bool
	for _, id := range api.removeIDs {
		if id == "incumbent-id" {
			removedIncumbent = true
		}
	}
	if !removedIncumbent {
		t.Errorf("the incumbent was never removed; removes = %v", api.removeIDs)
	}
}

// TestContainerBackend_RunWorkspaceInit_NameConflictEngineErrorInTheWaitResponseRefusesToRemove
// is the conflict path's half of the same omission.
//
// This wait exists to establish ONE fact — the incumbent is no longer running —
// because the next statement force removes it, and the incumbent is by
// construction the container most likely to be tens of minutes into a toolchain
// install against this very home. An engine that answers the wait with an error
// established nothing, so proceeding to the remove is the "SIGKILL a 25-minute
// download" mistake §D6 exists to prevent, taken on the strength of a field
// nobody read.
//
// Note what the old code did here: it selected on waitRes.Result and DISCARDED
// the value, so there was not even a StatusCode to reason about.
func TestContainerBackend_RunWorkspaceInit_NameConflictEngineErrorInTheWaitResponseRefusesToRemove(t *testing.T) {
	const engineMessage = "runtime wait failed"
	var creates int
	api := &fakeDockerAPI{}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		creates++
		if creates == 1 {
			return client.ContainerCreateResult{}, fakeConflictError{}
		}
		return client.ContainerCreateResult{ID: "fresh"}, nil
	}
	api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
		return incumbentInitContainerInspect("incumbent-id", true), nil
	}
	api.ContainerWaitFunc = waitResponding(container.WaitResponse{
		StatusCode: 0,
		Error:      &container.WaitExitError{Message: engineMessage},
	})
	b := newWorkspaceInitBackend(api)

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	if err == nil {
		t.Fatal("RunWorkspaceInit returned nil after the engine answered the incumbent's wait with an error")
	}
	if msg := err.Error(); !strings.Contains(msg, engineMessage) {
		t.Errorf("the error does not carry the engine's own message %q:\n%v", engineMessage, err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	for _, id := range api.removeIDs {
		if id == "incumbent-id" {
			t.Error("the incumbent was force removed even though the wait never established that it had stopped; it may have been mid-install into this workspace's home")
		}
	}
	if creates != 1 {
		t.Errorf("ContainerCreate called %d times, want 1: the run must stop at the unresolved conflict rather than retry into a home another container is still writing", creates)
	}
}

// TestContainerBackend_RunWorkspaceInit_NameConflictActsOnTheInspectedID pins
// that everything after the inspect is addressed by CONTAINER ID.
//
// A name is a mutable, reusable handle. Between "inspect the container holding
// this name" and "wait for it, then remove it" the incumbent can exit, be
// collected, and the name be taken by a NEW init container started by another
// daemon — at which point a remove by name destroys a container that was never
// inspected and may be minutes into a toolchain install. Pinning to the id the
// inspect returned makes the whole sequence refer to one individual: if that
// individual is gone the remove 404s harmlessly instead of hitting its
// successor.
func TestContainerBackend_RunWorkspaceInit_NameConflictActsOnTheInspectedID(t *testing.T) {
	name := dockerres.WorkspaceInitContainerName(testWorkspaceInitInstallID, "myws")
	var creates int
	api := &fakeDockerAPI{}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		creates++
		if creates == 1 {
			return client.ContainerCreateResult{}, fakeConflictError{}
		}
		return client.ContainerCreateResult{ID: "fresh"}, nil
	}
	api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
		return incumbentInitContainerInspect("incumbent-id", true), nil
	}
	api.ContainerWaitFunc = exitWith(0)
	b := newWorkspaceInitBackend(api)

	if err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest()); err != nil {
		t.Fatalf("RunWorkspaceInit: %v", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// The name is used for exactly one thing: the create, where it IS the
	// exclusion mechanism.
	if len(api.inspectIDs) == 0 || api.inspectIDs[0] != name {
		t.Errorf("inspects = %v, want the first one to resolve the name %q", api.inspectIDs, name)
	}
	for _, id := range api.waitIDs {
		if id == name {
			t.Errorf("waited on the NAME %q; a name can be re-bound to a different container between the inspect and the wait", name)
		}
	}
	for _, id := range api.removeIDs {
		if id == name {
			t.Errorf("removed by the NAME %q; if the incumbent exited and another daemon re-created the name, this destroys that container instead", name)
		}
	}
	var sawIncumbentWait, sawIncumbentRemove bool
	for _, id := range api.waitIDs {
		if id == "incumbent-id" {
			sawIncumbentWait = true
		}
	}
	for _, id := range api.removeIDs {
		if id == "incumbent-id" {
			sawIncumbentRemove = true
		}
	}
	if !sawIncumbentWait || !sawIncumbentRemove {
		t.Errorf("wait/remove did not target the inspected id: waits = %v removes = %v", api.waitIDs, api.removeIDs)
	}
}

// TestContainerBackend_RunWorkspaceInit_InspectFailureIsNotAVanishedContainer
// is the core of the §D6 safety property, and the case the first version of
// this code got wrong: it treated EVERY inspect error as "the incumbent is
// gone" and went straight to a force remove.
//
// The failure that produces is not hypothetical. An init container is
// routinely tens of minutes into a ~1.5GB toolchain download; an engine under
// that load answers a `GET /containers/<name>/json` with a 500 or times out
// the socket often enough to matter. Reading that as "gone" and force-removing
// SIGKILLs the very install this run is waiting for, and the retry then re-runs
// init.sh against a half-populated home — the exact outcome §D6 exists to
// prevent ("running なら完了を待つ。force kill しない").
//
// So: only errdefs.IsNotFound means gone (the same predicate internal/reap
// uses). Anything else is UNKNOWN STATE, and boid does not destroy things whose
// state it does not know.
func TestContainerBackend_RunWorkspaceInit_InspectFailureIsNotAVanishedContainer(t *testing.T) {
	var creates int
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		creates++
		if creates == 1 {
			return client.ContainerCreateResult{}, fakeConflictError{}
		}
		return client.ContainerCreateResult{ID: "fresh"}, nil
	}
	api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
		return client.ContainerInspectResult{}, errors.New("Error response from daemon: 500 Internal Server Error")
	}
	b := newWorkspaceInitBackend(api)

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	if err == nil {
		t.Fatal("RunWorkspaceInit succeeded despite being unable to determine the incumbent container's state")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.removeIDs) != 0 {
		t.Errorf("removed %v while the incumbent's state was unknown; it may be mid-install", api.removeIDs)
	}
	if creates != 1 {
		t.Errorf("ContainerCreate called %d times, want 1: the run must not retry past an undetermined conflict", creates)
	}
	for _, want := range []string{"500 Internal Server Error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not carry the engine's own message %q:\n%v", want, err)
		}
	}
}

// TestContainerBackend_RunWorkspaceInit_NameConflictWithAVanishedContainer is
// the one inspect error that IS safe to read as "gone": a genuine 404. The
// incumbent was collected between the create and the inspect (another daemon's
// startup sweep, an operator's `docker rm`), so there is nothing to wait for
// and nothing to remove — just retry the create.
func TestContainerBackend_RunWorkspaceInit_NameConflictWithAVanishedContainer(t *testing.T) {
	var creates int
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		creates++
		if creates == 1 {
			return client.ContainerCreateResult{}, fakeConflictError{}
		}
		return client.ContainerCreateResult{ID: "fresh"}, nil
	}
	api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
		return client.ContainerInspectResult{}, fakeNotFoundError{}
	}
	b := newWorkspaceInitBackend(api)

	if err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest()); err != nil {
		t.Fatalf("RunWorkspaceInit: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if creates != 2 {
		t.Errorf("ContainerCreate called %d times, want 2 (the conflict, then the retry)", creates)
	}
	for _, id := range api.removeIDs {
		if id != "fresh" {
			t.Errorf("removed %q, but the incumbent was already gone; there is no id to remove", id)
		}
	}
}

// TestContainerBackend_RunWorkspaceInit_RefusesToRemoveAContainerBoidDidNotCreate
// pins the ownership check.
//
// The deterministic name is not reserved by anything. `docker run --name
// boid-ws-init-<install8>-<slug> ...` succeeds for anyone, and so does a
// sanitized name collision from an unrelated tool. Without a label check the
// conflict path would force-remove whatever holds the name — turning "boid
// could not prepare a workspace home" into "boid destroyed somebody else's
// container", which is both worse and silent.
func TestContainerBackend_RunWorkspaceInit_RefusesToRemoveAContainerBoidDidNotCreate(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"no labels at all", nil},
		{"unrelated labels", map[string]string{"com.example.app": "postgres"}},
		{
			// A container carrying boid's JOB labels is still not this
			// container: removing a running job's container would kill the job.
			"a boid job container that happens to hold the name",
			map[string]string{labelJobID: "job-1", labelInstallID: testWorkspaceInitInstallID},
		},
		{
			// Another installation's init container. The install id is only 8
			// characters of the name, so two installs CAN compute the same
			// name, and the label is what tells them apart.
			"another installation's init container",
			map[string]string{
				dockerres.LabelWorkspaceInit:          "myws",
				dockerres.LabelWorkspaceInitInstallID: "ffffffffffffffff",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var creates int
			api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
			api.ContainerAttachFunc = attachThenEOF(api, "")
			api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
				creates++
				if creates == 1 {
					return client.ContainerCreateResult{}, fakeConflictError{}
				}
				return client.ContainerCreateResult{ID: "fresh"}, nil
			}
			api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
				return client.ContainerInspectResult{
					Container: container.InspectResponse{
						ID:     "foreign-id",
						State:  &container.State{Running: false},
						Config: &container.Config{Labels: tc.labels},
					},
				}, nil
			}
			b := newWorkspaceInitBackend(api)

			err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
			if err == nil {
				t.Fatal("RunWorkspaceInit succeeded against a name held by a container boid did not create")
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if len(api.removeIDs) != 0 {
				t.Errorf("removed %v; a container without boid's own init label must never be touched", api.removeIDs)
			}
			if creates != 1 {
				t.Errorf("ContainerCreate called %d times, want 1", creates)
			}
			// The operator has to be able to read "the name is taken, and not
			// by me" off the message — that is a different action (rename or
			// remove the other container) from every other failure here.
			msg := err.Error()
			wantName := dockerres.WorkspaceInitContainerName(testWorkspaceInitInstallID, "myws")
			if !strings.Contains(msg, wantName) {
				t.Errorf("error does not name the contended container %q:\n%s", wantName, msg)
			}
			if !strings.Contains(msg, dockerres.LabelWorkspaceInit) {
				t.Errorf("error does not say which label was missing (%q), so the operator cannot tell whose container it is:\n%s",
					dockerres.LabelWorkspaceInit, msg)
			}
		})
	}
}

// TestContainerBackend_RunWorkspaceInit_NameConflictWithAnExitedContainer
// covers the ordinary leftover: a container that already exited is
// unambiguous garbage, so it is removed without waiting.
func TestContainerBackend_RunWorkspaceInit_NameConflictWithAnExitedContainer(t *testing.T) {
	var creates int
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		creates++
		if creates == 1 {
			return client.ContainerCreateResult{}, fakeConflictError{}
		}
		return client.ContainerCreateResult{ID: "fresh"}, nil
	}
	api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
		return incumbentInitContainerInspect("incumbent-id", false), nil
	}
	b := newWorkspaceInitBackend(api)

	if err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest()); err != nil {
		t.Fatalf("RunWorkspaceInit: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if creates != 2 {
		t.Errorf("ContainerCreate called %d times, want 2", creates)
	}
	// The wait on the incumbent must NOT have happened; waits are only for
	// the fresh container.
	for _, id := range api.waitIDs {
		if id != "fresh" {
			t.Errorf("waited on %q, but an exited incumbent needs no waiting", id)
		}
	}
}

// conflictingBackend wires up the shape every ownership/state test below
// needs: the first create conflicts, the inspect that resolves the name
// answers with `insp`, and a second create would succeed. Whether that second
// create ever happens is what each test asserts on.
func conflictingBackend(insp client.ContainerInspectResult) (*fakeDockerAPI, *containerBackend, *int) {
	creates := new(int)
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		*creates++
		if *creates == 1 {
			return client.ContainerCreateResult{}, fakeConflictError{}
		}
		return client.ContainerCreateResult{ID: "fresh"}, nil
	}
	api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
		return insp, nil
	}
	return api, newWorkspaceInitBackend(api), creates
}

// assertRefusedTheIncumbent is the shared assertion for every case where boid
// must NOT touch the container holding the name: an error out, nothing
// removed, and no retry of the create (a retry past an unresolved conflict is
// how a second init ends up writing into a home the first one is still
// installing into).
func assertRefusedTheIncumbent(t *testing.T, api *fakeDockerAPI, creates *int, err error, wantInErr ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("RunWorkspaceInit succeeded against a name it must have refused to clear")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.removeIDs) != 0 {
		t.Errorf("removed %v; nothing may be destroyed on this path", api.removeIDs)
	}
	if *creates != 1 {
		t.Errorf("ContainerCreate called %d times, want 1: the run must not retry past an unresolved conflict", *creates)
	}
	for _, want := range wantInErr {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so the operator cannot act on it:\n%v", want, err)
		}
	}
}

// TestContainerBackend_RunWorkspaceInit_RefusesAnIncumbentWithNoID pins the
// guard added in the first codex round, which no test addressed directly.
//
// Everything after the inspect is addressed by container ID precisely so a
// re-bound name cannot redirect a wait or a remove onto a different container
// (see TestContainerBackend_RunWorkspaceInit_NameConflictActsOnTheInspectedID).
// An inspect that answers with an EMPTY id takes that away: the only handle
// left is the name, i.e. exactly the handle the id-pinning exists to avoid. So
// this is an unknown state like any other, and boid refuses rather than
// falling back to the name.
func TestContainerBackend_RunWorkspaceInit_RefusesAnIncumbentWithNoID(t *testing.T) {
	api, b, creates := conflictingBackend(client.ContainerInspectResult{
		Container: container.InspectResponse{
			ID:    "",
			State: &container.State{Running: false},
			Config: &container.Config{Labels: map[string]string{
				dockerres.LabelWorkspaceInit:          "myws",
				dockerres.LabelWorkspaceInitInstallID: testWorkspaceInitInstallID,
			}},
		},
	})

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	assertRefusedTheIncumbent(t, api, creates, err,
		dockerres.WorkspaceInitContainerName(testWorkspaceInitInstallID, "myws"))
}

// TestContainerBackend_RunWorkspaceInit_RefusesAnIncumbentWithUnknownState is
// the second codex round's blocker (b).
//
// container.InspectResponse.State is a POINTER, and a nil one is not "stopped"
// — it is "the engine did not tell us". Reading it as stopped is the same
// class of mistake as reading a 500 from the inspect as "gone" (see
// TestContainerBackend_RunWorkspaceInit_InspectFailureIsNotAVanishedContainer):
// it converts missing information into a force remove of a container that may
// be tens of minutes into a ~1.5GB toolchain install into this very home.
//
// §D6's rule is "running なら完了を待つ。force kill しない", and the only way to
// honour it is to know whether it is running. When boid cannot know, it stops.
func TestContainerBackend_RunWorkspaceInit_RefusesAnIncumbentWithUnknownState(t *testing.T) {
	api, b, creates := conflictingBackend(client.ContainerInspectResult{
		Container: container.InspectResponse{
			ID:    "incumbent-id",
			State: nil,
			Config: &container.Config{Labels: map[string]string{
				dockerres.LabelWorkspaceInit:          "myws",
				dockerres.LabelWorkspaceInitInstallID: testWorkspaceInitInstallID,
			}},
		},
	})

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	assertRefusedTheIncumbent(t, api, creates, err, "incumbent-id")
	if err != nil {
		api.mu.Lock()
		waits := append([]string(nil), api.waitIDs...)
		api.mu.Unlock()
		for _, id := range waits {
			if id == "incumbent-id" {
				t.Error("waited on an incumbent whose state is unknown; a wait is only correct once boid knows it is running")
			}
		}
	}
}

// TestContainerBackend_RunWorkspaceInit_RefusesAnIncumbentForADifferentWorkspace
// is the second codex round's blocker (a).
//
// The earlier version of verifyWorkspaceInitIncumbent checked only that the
// boid.workspace_init KEY was present, on the argument that the name is a pure
// function of (installIDPart, slug) so the value could not disagree. That
// argument is circular: it assumes the incumbent was produced by that function,
// which is the very thing being checked. Two inputs break it without any
// exotic scenario —
//
//   - `docker rename` (or podman's) on another workspace's init container. The
//     name moves; the labels do not.
//   - anything at all that creates this name with a boid.workspace_init label
//     for a different slug, which requires no privileges beyond `docker run`.
//
// In both cases the pre-fix code force-removed ANOTHER WORKSPACE's in-flight
// init. Comparing the value costs nothing and makes the check say what it
// claims to say.
func TestContainerBackend_RunWorkspaceInit_RefusesAnIncumbentForADifferentWorkspace(t *testing.T) {
	api, b, creates := conflictingBackend(client.ContainerInspectResult{
		Container: container.InspectResponse{
			ID:    "other-workspace-init",
			State: &container.State{Running: false},
			Config: &container.Config{Labels: map[string]string{
				// Same installation, same key, DIFFERENT workspace.
				dockerres.LabelWorkspaceInit:          "otherws",
				dockerres.LabelWorkspaceInitInstallID: testWorkspaceInitInstallID,
			}},
		},
	})

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	assertRefusedTheIncumbent(t, api, creates, err, "otherws", "myws")
}

// TestContainerBackend_RunWorkspaceInit_SecondConflictFailsLoud pins §D6's
// last clause. Two conflicts in a row means something is creating this name
// concurrently and the run cannot establish exclusivity — proceeding anyway
// would be exactly the concurrent-init the name exists to prevent.
func TestContainerBackend_RunWorkspaceInit_SecondConflictFailsLoud(t *testing.T) {
	api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
	api.ContainerAttachFunc = attachThenEOF(api, "")
	api.ContainerCreateFunc = func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		return client.ContainerCreateResult{}, fakeConflictError{}
	}
	api.ContainerInspectFunc = func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
		return incumbentInitContainerInspect("incumbent-id", false), nil
	}
	b := newWorkspaceInitBackend(api)

	err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest())
	if err == nil {
		t.Fatal("RunWorkspaceInit succeeded despite never obtaining the exclusive name")
	}
	if name := dockerres.WorkspaceInitContainerName(testWorkspaceInitInstallID, "myws"); !strings.Contains(err.Error(), name) {
		t.Errorf("error does not name the contended container %q:\n%v", name, err)
	}
}

// incumbentInitContainerInspect builds the ContainerInspect answer for a
// LEGITIMATE incumbent: one of this install's own workspace init containers,
// holding the deterministic name. Every field it sets is load-bearing — the
// id because the conflict path addresses the container by id, the labels
// because it verifies ownership before removing anything.
func incumbentInitContainerInspect(id string, running bool) client.ContainerInspectResult {
	return client.ContainerInspectResult{
		Container: container.InspectResponse{
			ID:    id,
			State: &container.State{Running: running},
			Config: &container.Config{Labels: map[string]string{
				dockerres.LabelWorkspaceInit:          "myws",
				dockerres.LabelWorkspaceInitInstallID: testWorkspaceInitInstallID,
			}},
		},
	}
}

// fakeNotFoundError satisfies errdefs.IsNotFound through the same
// `interface{ NotFound() }` the moby client's own 404s do.
type fakeNotFoundError struct{}

func (fakeNotFoundError) Error() string { return "Error response from daemon: No such container" }
func (fakeNotFoundError) NotFound()     {}

// TestContainerBackend_RunWorkspaceInit_RootlessPodmanKeepsTheHostUIDMapping
// pins §D7's second half. Without keep-id, everything the prep creates lands
// on the host owned by a subuid, and PR3's prepareBindTarget rejects the very
// directories this run just made — every dispatch afterwards fails with the
// poisoned-bind-target error.
func TestContainerBackend_RunWorkspaceInit_RootlessPodmanKeepsTheHostUIDMapping(t *testing.T) {
	api := &fakeDockerAPI{
		ContainerWaitFunc: exitWith(0),
		ServerVersionFunc: func(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
			return client.ServerVersionResult{
				Components: []system.ComponentVersion{{Name: "Podman Engine", Version: "4.9.3"}},
			}, nil
		},
		InfoFunc: func(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
			return client.SystemInfoResult{Info: system.Info{SecurityOptions: []string{"name=rootless"}}}, nil
		},
	}
	b := newWorkspaceInitBackend(api)

	if err := b.RunWorkspaceInit(context.Background(), testWorkspaceInitRequest()); err != nil {
		t.Fatalf("RunWorkspaceInit: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if got := api.createCalls[0].HostConfig.UsernsMode; got != keepIDUsernsMode {
		t.Errorf("UsernsMode = %q, want %q on rootless podman", got, keepIDUsernsMode)
	}
}

// TestContainerBackend_RunWorkspaceInit_RejectsAHomeSourceThatIsNotAVolumeName
// is the fail-closed guard on the one field PR6 changed, and its polarity is
// INVERTED from the PR5 version of this test on purpose.
//
// Through PR5 the home was a host path and a relative source had to be refused,
// because docker reads a relative mount source as a volume NAME and would have
// silently prepared a brand new empty volume. The home IS a volume name now, so
// the dangerous value is the other one: an absolute source would bind some host
// directory into the container, the init would report success, the marker would
// be stamped — and the workspace's real home would never be prepared. Both eras
// fail the same way when the two representations are confused, so the check
// tracks which one is current rather than disappearing.
func TestContainerBackend_RunWorkspaceInit_RejectsAHomeSourceThatIsNotAVolumeName(t *testing.T) {
	for _, tc := range []struct {
		name       string
		homeSource string
	}{
		{"an absolute host path (the pre-PR6 representation)", "/srv/boid/homes/myws"},
		{"a relative path that is not a valid volume name", "../escape"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeDockerAPI{ContainerWaitFunc: exitWith(0)}
			api.ContainerAttachFunc = attachThenEOF(api, "")
			b := newWorkspaceInitBackend(api)
			req := testWorkspaceInitRequest()
			req.HomeSource = tc.homeSource

			if err := b.RunWorkspaceInit(context.Background(), req); err == nil {
				t.Errorf("RunWorkspaceInit accepted home source %q; as of PR6 the home is a docker named volume", tc.homeSource)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if len(api.createCalls) != 0 {
				t.Error("a container was created despite the rejected mount source")
			}
			if len(api.volumeCreateCalls) != 0 {
				t.Errorf("a volume was created for the rejected source: %+v", api.volumeCreateCalls)
			}
		})
	}
}

// --- reap (§D5's second half) ----------------------------------------------

// TestContainerBackend_ReapOrphans_CollectsExitedInitContainers pins that
// "invisible to the job sweeps" does not mean "nobody's problem". A daemon
// crash leaves the init container behind holding the deterministic name; the
// next init would have to clear it, but only for that one workspace and only
// on its next dispatch, so startup reap collects them too.
//
// The other half of the assertion is the one that matters more: the sweep must
// not put anything into ReapReport. Those fields drive MarkStale and gate
// auto-reopen for every task of that boot.
func TestContainerBackend_ReapOrphans_CollectsExitedInitContainers(t *testing.T) {
	initName := dockerres.WorkspaceInitContainerName(testWorkspaceInitInstallID, "myws")
	api := &fakeDockerAPI{}
	api.ContainerListFunc = func(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
		if hasFilterValue(opts.Filters, "label", dockerres.LabelWorkspaceInit) {
			return client.ContainerListResult{Items: []container.Summary{{
				ID:    "init-container-id",
				Names: []string{"/" + initName},
				State: container.StateExited,
				Labels: map[string]string{
					dockerres.LabelWorkspaceInit:          "myws",
					dockerres.LabelWorkspaceInitInstallID: testWorkspaceInitInstallID,
				},
			}}}, nil
		}
		return client.ContainerListResult{}, nil
	}
	b := newWorkspaceInitBackend(api)

	report, err := b.ReapOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	api.mu.Lock()
	removed := append([]string(nil), api.removeIDs...)
	api.mu.Unlock()

	var found bool
	for _, id := range removed {
		if id == "init-container-id" {
			found = true
		}
	}
	if !found {
		t.Errorf("the orphaned init container was not collected; removes = %v", removed)
	}
	if len(report.ReapedJobIDs) != 0 || len(report.FailedJobIDs) != 0 {
		t.Errorf("report = %+v, want no job ids: an init container has no job, and these fields gate other tasks' auto-reopen", report)
	}
}

// TestContainerBackend_ReapOrphans_LeavesARunningInitContainerAlone pins the
// exclusion that makes the sweep safe to run at startup. A running init
// container is either this install's in-flight preparation or a previous
// daemon's — and either way it is partway through installing a multi-GB
// toolchain into the home the next dispatch is about to want. Killing it buys
// nothing: §D6's name conflict already stops a second init from racing it, and
// covers the wedged case with a bounded wait.
func TestContainerBackend_ReapOrphans_LeavesARunningInitContainerAlone(t *testing.T) {
	api := &fakeDockerAPI{}
	api.ContainerListFunc = func(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
		if hasFilterValue(opts.Filters, "label", dockerres.LabelWorkspaceInit) {
			return client.ContainerListResult{Items: []container.Summary{{
				ID:    "running-init",
				State: container.StateRunning,
				Labels: map[string]string{
					dockerres.LabelWorkspaceInit:          "myws",
					dockerres.LabelWorkspaceInitInstallID: testWorkspaceInitInstallID,
				},
			}}}, nil
		}
		return client.ContainerListResult{}, nil
	}
	b := newWorkspaceInitBackend(api)

	if _, err := b.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, id := range api.removeIDs {
		if id == "running-init" {
			t.Error("a RUNNING init container was removed by the startup sweep; it is mid-toolchain-install into a workspace home")
		}
	}
}

// TestContainerBackend_ReapOrphans_OnlyCollectsTerminalInitContainers is the
// second codex round's Major 2, generalized into the rule the sweep now
// follows: it is an ALLOWLIST of terminal states, not a denylist of live ones.
//
// The bug the allowlist replaces was `paused`. A paused container is not the
// residue of a finished run — it is a live process tree frozen mid-syscall,
// holding a half-finished toolchain install in its writable layer, and
// `ContainerRemove{Force}` throws all of it away. Restarting the daemon is
// enough to trigger it, and the same denylist would have mis-classified every
// state docker adds in the future.
//
// The states below are moby's own enumeration (container.ContainerState in
// github.com/moby/moby/api/types/container/state.go, the declared type of
// container.Summary.State) plus one string that is in no enumeration at all —
// because the reason to spell an allowlist is precisely that the unknown case
// exists.
func TestContainerBackend_ReapOrphans_OnlyCollectsTerminalInitContainers(t *testing.T) {
	cases := []struct {
		state       container.ContainerState
		wantRemoved bool
		why         string
	}{
		{container.StateExited, true, "the run finished; this is the residue the sweep exists for"},
		{container.StateDead, true, "the engine failed to delete it and retries on restart; there is no process left"},
		{container.StatePaused, false, "a frozen, LIVE process tree holding a half-finished toolchain install"},
		{container.StateRunning, false, "mid-install"},
		{container.StateRestarting, false, "about to be running again"},
		{container.StateRemoving, false, "the engine is already removing it; a second force remove races that"},
		{container.StateCreated, false, "it has not run yet — a concurrent daemon may be between its create and its start"},
		{"", false, "the engine reported no state at all"},
		{"stopping", false, "not in moby's enumeration; an unknown state must fall to the protected side"},
	}
	for _, tc := range cases {
		name := string(tc.state)
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			api := &fakeDockerAPI{}
			api.ContainerListFunc = func(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
				if hasFilterValue(opts.Filters, "label", dockerres.LabelWorkspaceInit) {
					return client.ContainerListResult{Items: []container.Summary{{
						ID:    "init-id",
						State: tc.state,
						Labels: map[string]string{
							dockerres.LabelWorkspaceInit:          "myws",
							dockerres.LabelWorkspaceInitInstallID: testWorkspaceInitInstallID,
						},
					}}}, nil
				}
				return client.ContainerListResult{}, nil
			}
			b := newWorkspaceInitBackend(api)

			if _, err := b.ReapOrphans(context.Background()); err != nil {
				t.Fatalf("ReapOrphans: %v", err)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			var removed bool
			for _, id := range api.removeIDs {
				if id == "init-id" {
					removed = true
				}
			}
			if removed != tc.wantRemoved {
				t.Errorf("state %q: removed = %v, want %v — %s", tc.state, removed, tc.wantRemoved, tc.why)
			}
		})
	}
}

// TestContainerBackend_ReapOrphans_LeavesAnotherInstallsInitContainerAlone
// keeps the sweep install-scoped, matching every other sweep in this backend:
// two boid installations sharing one engine must not collect each other's
// work.
func TestContainerBackend_ReapOrphans_LeavesAnotherInstallsInitContainerAlone(t *testing.T) {
	api := &fakeDockerAPI{}
	api.ContainerListFunc = func(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
		if hasFilterValue(opts.Filters, "label", dockerres.LabelWorkspaceInit) {
			return client.ContainerListResult{Items: []container.Summary{{
				ID:    "other-install-init",
				State: container.StateExited,
				Labels: map[string]string{
					dockerres.LabelWorkspaceInit:          "myws",
					dockerres.LabelWorkspaceInitInstallID: "ffffffffffffffff",
				},
			}}}, nil
		}
		return client.ContainerListResult{}, nil
	}
	b := newWorkspaceInitBackend(api)

	if _, err := b.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, id := range api.removeIDs {
		if id == "other-install-init" {
			t.Error("another installation's init container was removed")
		}
	}
}

// hasFilterValue reports whether f carries key=value. client.Filters is a
// plain map[string]map[string]bool, so this reads it directly.
func hasFilterValue(f client.Filters, key, value string) bool {
	return f[key][value]
}
