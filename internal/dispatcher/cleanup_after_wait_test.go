package dispatcher

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
)

// waitableSession is a minimal backend.SandboxSession whose Wait returns a
// configurable backend.RuntimeExit/error — used to drive
// Runner.cleanupSandboxAfterWait through the success and failure branches
// without a real containerBackend/docker daemon.
//
// PR-4 (docs/plans/volume-only-daemon.md §論点e) removed the userns
// backend's PreparedSandbox capability (usernsSession.localArtifacts) this
// file's tests used to drive via a fake JobRuntime (waitableRuntime) wrapped
// in a real usernsSession — cleanupSandboxAfterWait no longer touches any
// sandbox-local artifact files at all (that responsibility now belongs
// entirely to whichever backend produced the session, e.g.
// containerSession.waitLoop, covered by container_backend_test.go), so this
// file only needs to drive the session.Wait/reapAndCloseDockerProxy/extra
// callback behavior that IS still Runner.cleanupSandboxAfterWait's own.
type waitableSession struct {
	id   string
	exit backend.RuntimeExit
	err  error
}

var _ backend.SandboxSession = (*waitableSession)(nil)

func (s *waitableSession) ID() string { return s.id }
func (s *waitableSession) Subscribe() ([]byte, <-chan []byte, func(), bool) {
	return nil, nil, func() {}, false
}
func (s *waitableSession) WriteInput([]byte) error           { return ErrRuntimeUnsupported }
func (s *waitableSession) CloseInput() error                 { return ErrRuntimeUnsupported }
func (s *waitableSession) Resize(backend.TerminalSize) error { return ErrRuntimeUnsupported }
func (s *waitableSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return s.exit, s.err
}
func (s *waitableSession) Stop(context.Context) error                   { return nil }
func (s *waitableSession) Signal(context.Context, syscall.Signal) error { return nil }

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
			called := false
			r := &Runner{}

			r.cleanupSandboxAfterWait(&waitableSession{id: "rt-x", exit: tc.exit}, func() { called = true })

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
				c.Read(buf)                                                             //nolint:errcheck
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

	_, ds := startFakeDockerProxy(t, dir)

	r := &Runner{
		dockerStates: map[string]*dockerProxyState{"rt-docker-ok": ds},
	}

	r.cleanupSandboxAfterWait(&waitableSession{id: "rt-docker-ok", exit: RuntimeExit{ExitCode: 0}}, nil)

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

	_, ds := startFakeDockerProxy(t, dir)

	r := &Runner{
		dockerStates: map[string]*dockerProxyState{"rt-docker-fail": ds},
	}

	r.cleanupSandboxAfterWait(&waitableSession{id: "rt-docker-fail", exit: RuntimeExit{ExitCode: 1}}, nil)

	r.dockerMu.Lock()
	_, stillPresent := r.dockerStates["rt-docker-fail"]
	r.dockerMu.Unlock()
	if stillPresent {
		t.Error("dockerState should be removed from map even on exit_code!=0")
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
