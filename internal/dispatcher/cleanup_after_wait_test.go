package dispatcher

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
)

// waitableRuntime is a JobRuntime stub whose Wait returns a configurable
// RuntimeExit. Used to drive cleanupSandboxAfterWait through the success and
// failure branches.
type waitableRuntime struct {
	exit RuntimeExit
	err  error
}

func (r *waitableRuntime) Start(_ context.Context, _ RuntimeStartSpec) (*RuntimeHandle, error) {
	return nil, ErrRuntimeUnsupported
}
func (r *waitableRuntime) Attach(_ context.Context, _ string, _ RuntimeAttachRequest) error {
	return ErrRuntimeUnsupported
}
func (r *waitableRuntime) Resize(_ context.Context, _ string, _ TerminalSize) error {
	return ErrRuntimeUnsupported
}
func (r *waitableRuntime) Wait(_ context.Context, _ string) (RuntimeExit, error) {
	return r.exit, r.err
}
func (r *waitableRuntime) Stop(_ context.Context, _ string) error {
	return nil
}
func (r *waitableRuntime) Signal(_ context.Context, _ string, _ syscall.Signal) error {
	return nil
}

func makePreparedFixture(t *testing.T) *PreparedSandbox {
	t.Helper()
	dir := t.TempDir()

	rootDir := filepath.Join(dir, "boid-root-XXX")
	stagingDir := filepath.Join(dir, "boid-staging-YYY")
	specPath := filepath.Join(dir, "boid-job-runner-spec.json")
	statePath := filepath.Join(dir, "boid-job-runner-state.json")

	for _, d := range []string{rootDir, stagingDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for _, f := range []string{specPath, statePath} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return &PreparedSandbox{
		SpecPath:   specPath,
		StatePath:  statePath,
		RootDir:    rootDir,
		StagingDir: stagingDir,
	}
}

// sessionFor builds a usernsSession wrapping runtime/runtimeID/prepared —
// the same shape Runner.launchSandbox receives back from
// SandboxBackend.Launch — so these tests can drive
// Runner.cleanupSandboxAfterWait / Runner.watchRuntime through the
// backend.SandboxSession interface exactly as production code does, rather
// than through dispatcher-internal (runtimeID, *PreparedSandbox) plumbing
// that no longer exists post-Phase-6-PR1.
func sessionFor(runtime JobRuntime, runtimeID string, prepared *PreparedSandbox) *usernsSession {
	return &usernsSession{runtime: runtime, id: runtimeID, prepared: prepared}
}

func TestCleanupSandboxAfterWait_RemovesArtifactsOnSuccess(t *testing.T) {
	prep := makePreparedFixture(t)
	r := &Runner{Runtime: &waitableRuntime{exit: RuntimeExit{ExitCode: 0}}}

	r.cleanupSandboxAfterWait(sessionFor(r.Runtime, "rt-success", prep), nil)

	for _, p := range []string{prep.RootDir, prep.StagingDir, prep.SpecPath, prep.StatePath} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s removed on exit_code=0, stat err = %v", p, err)
		}
	}
}

// silent exit_code=1 の事後解析を可能にするため、 失敗時は **runner-state.json
// だけ** 残す。 rootDir / stagingDir は保全しても診断材料にならず削除し、 secrets を
// 抱える spec ファイルも常に削除する。 redact 済みの runner-state.json だけが残る。
func TestCleanupSandboxAfterWait_RetainsStateOnFailure(t *testing.T) {
	prep := makePreparedFixture(t)
	r := &Runner{Runtime: &waitableRuntime{exit: RuntimeExit{ExitCode: 1}}}

	r.cleanupSandboxAfterWait(sessionFor(r.Runtime, "rt-failed", prep), nil)

	// Scaffolding + spec (secrets) must be removed even on failure.
	for _, p := range []string{prep.RootDir, prep.StagingDir, prep.SpecPath} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s removed on exit_code!=0, stat err = %v", p, err)
		}
	}
	// runner-state.json は事後解析のため保全する。
	if _, err := os.Stat(prep.StatePath); err != nil {
		t.Errorf("expected runner-state %s retained on exit_code!=0, stat err = %v", prep.StatePath, err)
	}
}

func TestTranscriptSizeBytes(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	withData := filepath.Join(dir, "data.log")
	if err := os.WriteFile(withData, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}

	if size, msg := transcriptSizeBytes(""); size != -1 || msg == "" {
		t.Errorf("empty path: got (%d,%q), want (-1, non-empty)", size, msg)
	}
	if size, msg := transcriptSizeBytes(filepath.Join(dir, "missing.log")); size != -1 || msg == "" {
		t.Errorf("missing path: got (%d,%q), want (-1, non-empty)", size, msg)
	}
	if size, msg := transcriptSizeBytes(empty); size != 0 || msg != "" {
		t.Errorf("empty file: got (%d,%q), want (0,'')", size, msg)
	}
	if size, msg := transcriptSizeBytes(withData); size != 5 || msg != "" {
		t.Errorf("5-byte file: got (%d,%q), want (5,'')", size, msg)
	}
}

func TestCleanupSandboxAfterWait_RunsExtraCleanupAlways(t *testing.T) {
	cases := []struct {
		name string
		exit RuntimeExit
	}{
		{"success", RuntimeExit{ExitCode: 0}},
		{"failure", RuntimeExit{ExitCode: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prep := makePreparedFixture(t)
			called := false
			r := &Runner{Runtime: &waitableRuntime{exit: tc.exit}}

			r.cleanupSandboxAfterWait(sessionFor(r.Runtime, "rt-x", prep), func() { called = true })

			if !called {
				t.Errorf("extra cleanup must run regardless of exit code (case=%s)", tc.name)
			}
		})
	}
}

// fakeDockerUpstream is a minimal fake Unix-socket server that responds to
// docker API stop/rm requests with 204 No Content so dockerproxy.Reap can
// complete without a real docker daemon.
func startFakeDockerUpstream(t *testing.T, socketPath string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen fake docker upstream: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Read (and discard) the incoming HTTP request, then respond 204.
				buf := make([]byte, 4096)
				c.Read(buf) //nolint:errcheck
				c.Write([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")) //nolint:errcheck
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// startFakeDockerProxy creates a dockerproxy.Server backed by a fake upstream
// socket and a real ledger, then starts it. Returns the proxy state, the proxy
// socket path, and a channel that receives true when reapAndCloseDockerProxy
// drains the ledger (via proxy.Close causing Serve to return).
func startFakeDockerProxy(t *testing.T, runtimeDir string) (string, *dockerProxyState) {
	t.Helper()
	upstreamSock := filepath.Join(runtimeDir, "upstream.sock")
	startFakeDockerUpstream(t, upstreamSock)

	proxySock := filepath.Join(runtimeDir, "docker-proxy.sock")
	ln, err := net.Listen("unix", proxySock)
	if err != nil {
		t.Fatalf("listen proxy socket: %v", err)
	}
	ledgerPath := filepath.Join(runtimeDir, "docker-resources.jsonl")
	ledger := dockerproxy.NewLedger(ledgerPath)
	proxy := dockerproxy.NewWithLedger(upstreamSock, ledger)
	go proxy.Serve(ln) //nolint:errcheck

	return proxySock, &dockerProxyState{
		proxy:      proxy,
		listener:   ln,
		upstream:   upstreamSock,
		socketPath: proxySock,
		ledger:     ledger,
	}
}

// TestCleanupSandboxAfterWait_ReapsDockerOnSuccess verifies that docker Reap
// is called when the sandbox exits with code 0.
func TestCleanupSandboxAfterWait_ReapsDockerOnSuccess(t *testing.T) {
	dir := t.TempDir()
	prep := makePreparedFixture(t)

	_, ds := startFakeDockerProxy(t, dir)

	r := &Runner{
		Runtime:      &waitableRuntime{exit: RuntimeExit{ExitCode: 0}},
		dockerStates: map[string]*dockerProxyState{"rt-docker-ok": ds},
	}

	r.cleanupSandboxAfterWait(sessionFor(r.Runtime, "rt-docker-ok", prep), nil)

	// The proxy should have been removed from the map.
	r.dockerMu.Lock()
	_, stillPresent := r.dockerStates["rt-docker-ok"]
	r.dockerMu.Unlock()
	if stillPresent {
		t.Error("dockerState should be removed from map after cleanupSandboxAfterWait")
	}
}

// TestCleanupSandboxAfterWait_ReapsDockerOnFailure verifies that docker Reap
// is called even when the sandbox exits with a non-zero exit code.
func TestCleanupSandboxAfterWait_ReapsDockerOnFailure(t *testing.T) {
	dir := t.TempDir()
	prep := makePreparedFixture(t)

	_, ds := startFakeDockerProxy(t, dir)

	r := &Runner{
		Runtime:      &waitableRuntime{exit: RuntimeExit{ExitCode: 1}},
		dockerStates: map[string]*dockerProxyState{"rt-docker-fail": ds},
	}

	r.cleanupSandboxAfterWait(sessionFor(r.Runtime, "rt-docker-fail", prep), nil)

	r.dockerMu.Lock()
	_, stillPresent := r.dockerStates["rt-docker-fail"]
	r.dockerMu.Unlock()
	if stillPresent {
		t.Error("dockerState should be removed from map even on exit_code!=0")
	}
}

// TestCleanupSandboxAfterWait_ReapsDockerForSessionWithNoLocalArtifacts pins
// [Major 6, PR7 codex review]: a session with no PreparedSandbox (nil
// sessionLocalArtifacts — the shape every containerSession has, since it
// carries no userns scaffolding/spec/state files at all) must still get its
// docker proxy reaped and closed. Before this fix, cleanupSandboxAfterWait
// bailed out entirely (before ever calling session.Wait) whenever
// sessionLocalArtifacts returned nil, so a docker-enabled container-backend
// job's sibling resources were never reaped and its per-sandbox dockerproxy
// server was never closed — this test drives the exact same "prepared==nil"
// shape via sessionFor(..., nil) without needing a real containerSession /
// fake dockerAPI, since sessionLocalArtifacts' nil-vs-non-nil behavior is
// determined solely by the PreparedSandbox pointer, not by which concrete
// SandboxSession type carries it.
func TestCleanupSandboxAfterWait_ReapsDockerForSessionWithNoLocalArtifacts(t *testing.T) {
	dir := t.TempDir()
	_, ds := startFakeDockerProxy(t, dir)

	r := &Runner{
		Runtime:      &waitableRuntime{exit: RuntimeExit{ExitCode: 0}},
		dockerStates: map[string]*dockerProxyState{"rt-no-local-artifacts": ds},
	}

	// sessionFor(..., nil) mirrors sessionLocalArtifacts(containerSession)'s
	// nil return — see the type's own doc comment.
	r.cleanupSandboxAfterWait(sessionFor(r.Runtime, "rt-no-local-artifacts", nil), nil)

	r.dockerMu.Lock()
	_, stillPresent := r.dockerStates["rt-no-local-artifacts"]
	r.dockerMu.Unlock()
	if stillPresent {
		t.Error("dockerState should be removed from map even for a session with no local (userns) artifacts — reapAndCloseDockerProxy must still run")
	}
}

// TestStartDockerProxy_SocketPermissions verifies that the proxy socket file
// is created with 0600 permissions (owner-only access).
func TestStartDockerProxy_SocketPermissions(t *testing.T) {
	dir := t.TempDir()

	// Create a fake upstream socket so ResolveUpstream won't fail.
	upstreamSock := filepath.Join(dir, "docker.sock")
	startFakeDockerUpstream(t, upstreamSock)

	t.Setenv("DOCKER_HOST", "unix://"+upstreamSock)

	r := &Runner{RuntimesDir: dir}
	runtimeID := "test-perm-runtime"
	ds, err := r.startDockerProxy(runtimeID)
	if err != nil {
		t.Fatalf("startDockerProxy: %v", err)
	}
	t.Cleanup(func() { ds.proxy.Close() })

	info, err := os.Stat(ds.socketPath)
	if err != nil {
		t.Fatalf("stat proxy socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("proxy socket permissions = %04o, want 0600", perm)
	}
}

// TestValidateDockerHostCommands verifies that unrestricted docker host_commands
// registration is rejected when docker proxy is active, but subcommand-restricted
// registrations are accepted.
func TestValidateDockerHostCommands(t *testing.T) {
	cases := []struct {
		name    string
		cmds    map[string]orchestrator.CommandDef
		wantErr bool
	}{
		{
			name:    "no docker in host_commands",
			cmds:    map[string]orchestrator.CommandDef{"gh": {}},
			wantErr: false,
		},
		{
			name: "full docker access (no subcommands)",
			cmds: map[string]orchestrator.CommandDef{
				"docker": {AllowedSubcommands: nil, AllowedPatterns: nil},
			},
			wantErr: true,
		},
		{
			name: "docker with subcommand restriction",
			cmds: map[string]orchestrator.CommandDef{
				"docker": {AllowedSubcommands: []string{"build"}},
			},
			wantErr: false,
		},
		{
			name: "docker with pattern restriction",
			cmds: map[string]orchestrator.CommandDef{
				"docker": {AllowedPatterns: []string{"build *"}},
			},
			wantErr: false,
		},
		{
			name:    "empty host_commands",
			cmds:    nil,
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDockerHostCommands(tc.cmds)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateDockerHostCommands() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
