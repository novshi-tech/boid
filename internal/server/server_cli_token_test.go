package server_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/novshi-tech/boid/internal/server"
)

// TestCLIListener_RequiresBearerToken pins the daemon-side half of PR-3
// Option 4's host-mode redesign (docs/plans/volume-only-daemon.md §論点c):
// the dedicated CLI TCP listener (Config.CLIAddr/CLIToken) is only bound
// when BOTH are set, and every request against it — regardless of
// loopback-ness — requires the exact configured Bearer token. No TLS, no
// cookie, no loopback-trust bootstrap window: unlike the Web UI's own TCP
// listener (wire_tcp_auth_test.go's TestTCPListener_DataAPI_RequiresAuth),
// there is no "before first pairing" exemption here at all.
func TestCLIListener_RequiresBearerToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		CLIAddr:    "127.0.0.1:0",
		CLIToken:   "test-cli-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	cliAddr := srv.CLIListenAddr()
	if cliAddr == "" {
		t.Fatal("CLI listener should be open (CLIAddr and CLIToken were both set)")
	}

	cli := &http.Client{}

	// 1. No Authorization header at all: rejected.
	resp, err := cli.Get("http://" + cliAddr + "/api/tasks")
	if err != nil {
		t.Fatalf("GET /api/tasks (no auth): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth GET /api/tasks: status = %d, want 401", resp.StatusCode)
	}

	// 2. Wrong token: rejected.
	req, _ := http.NewRequest(http.MethodGet, "http://"+cliAddr+"/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = cli.Do(req)
	if err != nil {
		t.Fatalf("GET /api/tasks (wrong token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token GET /api/tasks: status = %d, want 401", resp.StatusCode)
	}

	// 3. Correct token: allowed.
	req, _ = http.NewRequest(http.MethodGet, "http://"+cliAddr+"/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer test-cli-token")
	resp, err = cli.Do(req)
	if err != nil {
		t.Fatalf("GET /api/tasks (correct token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("correct-token GET /api/tasks: got 401, want authenticated access")
	}

	// 4. /api/health stays public, no token needed.
	resp, err = cli.Get("http://" + cliAddr + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/health: status = %d, want 200 (public)", resp.StatusCode)
	}
}

// TestCLIListener_NotBoundWithoutToken pins the fail-closed contract: an
// empty CLIToken must skip binding the listener entirely, even when
// CLIAddr is set — there is no way to reach an unauthenticated instance of
// this listener.
func TestCLIListener_NotBoundWithoutToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		CLIAddr:    "127.0.0.1:0",
		// CLIToken deliberately unset.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	if addr := srv.CLIListenAddr(); addr != "" {
		t.Errorf("CLIListenAddr() = %q, want \"\" (no token configured, listener must not bind)", addr)
	}
}

// TestCLIListener_NotBoundWithoutAddr pins the symmetric case: a token
// with no CLIAddr also skips binding (nothing to bind to).
func TestCLIListener_NotBoundWithoutAddr(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		CLIToken:   "test-cli-token",
		// CLIAddr deliberately unset.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	if addr := srv.CLIListenAddr(); addr != "" {
		t.Errorf("CLIListenAddr() = %q, want \"\" (no CLIAddr configured, listener must not bind)", addr)
	}
}

// TestCLIListener_WSAttach_ReachesOverCLIToken is the end-to-end regression
// test for round-2 codex review Blocker 1: `boid attach`/`exec` (over
// BOID_MODE=container host mode) dial GET /api/jobs/{id}/attach/ws against
// this exact listener with `Authorization: Bearer <BOID_CLI_TOKEN>` — the
// bug was that WSAttachHandler.authenticateDevice re-verified that same
// header as a paired-device token and rejected it, even though
// NewCLITokenAuthMiddleware had already authenticated the request. The
// target job id does not exist, which is fine (see
// TestTCPListener_WSAttach_ReachableViaBearerAndCookie's own doc comment
// for why) — this test only cares that the handshake itself succeeds.
func TestCLIListener_WSAttach_ReachesOverCLIToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		CLIAddr:    "127.0.0.1:0",
		CLIToken:   "test-cli-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	cliAddr := srv.CLIListenAddr()
	if cliAddr == "" {
		t.Fatal("CLI listener should be open (CLIAddr and CLIToken were both set)")
	}

	wsURL := "ws://" + cliAddr + "/api/jobs/nonexistent-job/attach/ws"
	conn, resp, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer test-cli-token"}},
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("ws dial over CLI token listener: %v (status=%d)", err, status)
	}
	conn.CloseNow()
}

// TestCLIListener_ContainerBackend_BindsAllInterfaces pins the round-4 fix:
// under container backend (the only sandbox backend since PR-4, docs/plans/
// volume-only-daemon.md §論点e), the daemon runs inside a compose container
// reached via docker's bridge-mode port publish (`127.0.0.1:8442:8442` in
// build/container/compose.yml) — the host-side 127.0.0.1 is DNAT'd to the
// CONTAINER'S bridge-network IP, never its own loopback interface, so a
// listener bound to the container's 127.0.0.1 is unreachable from the host
// (the exact container-backend E2E failure this fix addresses: `GET
// http://127.0.0.1:8442/api/health = 000`). The fix reuses the same
// gatewayBindHost routing the git gateway/broker TLS listeners already use
// (wire_gateway_url_routing_test.go) instead of the CLIAddr listener's
// previous always-127.0.0.1 literal.
//
// PR-4 made this the ONLY behavior — a bare server.New() (no config.yaml,
// no Config.Backend override) already constructs a real containerBackend,
// so this test needs neither any more; TestCLIListener_UsernsBackend_
// StaysLoopback (the companion non-regression this test used to have) was
// removed outright along with the userns backend it pinned.
func TestCLIListener_ContainerBackend_BindsAllInterfaces(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	srv, err := server.New(server.Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		CLIAddr:    "127.0.0.1:0",
		CLIToken:   "test-cli-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	cliAddr := srv.CLIListenAddr()
	if cliAddr == "" {
		t.Fatal("CLI listener should be open (CLIAddr and CLIToken were both set)")
	}
	if strings.HasPrefix(cliAddr, "127.0.0.1:") {
		t.Errorf("CLIListenAddr() = %q, want NOT bound loopback-only under the container backend — docker's port-publish DNAT targets the container's bridge IP, not its own loopback", cliAddr)
	}
}
