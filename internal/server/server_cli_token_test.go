package server_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

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
