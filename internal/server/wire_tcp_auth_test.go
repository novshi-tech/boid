package server_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/server"
)

// TestTCPListener_DataAPI_RequiresAuth is the regression test for the
// transport-aware API auth fix: the data/control /api/* surface must be
// unauthenticated over the trusted UNIX socket (CLI/agent) but gated over the
// potentially-exposed TCP listener.
func TestTCPListener_DataAPI_RequiresAuth(t *testing.T) {
	// Isolate from the real $XDG_CONFIG_HOME (see testutil.NewTestServer's
	// doc comment) — this test constructs *server.Server directly rather
	// than via testutil.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	tcpAddr := srv.TCPAddr()
	if tcpAddr == "" {
		t.Fatal("TCP listener should be open")
	}

	// 1. UNIX socket is trusted: data API works without any session.
	unix := newTestClient(sockPath)
	resp, err := unix.Get("http://boid/api/tasks")
	if err != nil {
		t.Fatalf("unix GET /api/tasks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("unix GET /api/tasks: got 401, want trusted access (CLI must not be gated)")
	}

	tcp := &http.Client{}

	// 2. TCP request that arrived via a tunnel/proxy (X-Forwarded-For) gets no
	//    loopback bootstrap and must be rejected without a session.
	req, _ := http.NewRequest(http.MethodGet, "http://"+tcpAddr+"/api/tasks", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	resp, err = tcp.Do(req)
	if err != nil {
		t.Fatalf("tcp tunneled GET /api/tasks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tcp tunneled GET /api/tasks: status = %d, want 401", resp.StatusCode)
	}

	// 3. /api/health stays public over TCP even when tunneled.
	req, _ = http.NewRequest(http.MethodGet, "http://"+tcpAddr+"/api/health", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	resp, err = tcp.Do(req)
	if err != nil {
		t.Fatalf("tcp GET /api/health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("tcp GET /api/health: status = %d, want 200 (public)", resp.StatusCode)
	}

	// 4. Genuine local browser before pairing (loopback, no proxy header, no
	//    devices) keeps the bootstrap window so the Web UI is reachable.
	resp, err = tcp.Get("http://" + tcpAddr + "/api/tasks")
	if err != nil {
		t.Fatalf("tcp loopback GET /api/tasks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("tcp loopback (no devices) GET /api/tasks: got 401, want bootstrap pass")
	}
}

// newTestServerWithConfig writes config.yaml (under a fresh, isolated
// $XDG_CONFIG_HOME) with the given raw YAML body, then boots a *server.Server
// against it exactly like TestTCPListener_DataAPI_RequiresAuth above — shared
// by the web.loopback_trust tests below, which (unlike that test) need to
// control the config the daemon actually loads instead of relying on
// DefaultConfig().
func newTestServerWithConfig(t *testing.T, configYAML string) (srv *server.Server, tcpAddr string) {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "boid"), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "boid", "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	tmpDir := t.TempDir()
	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	tcpAddr = srv.TCPAddr()
	if tcpAddr == "" {
		t.Fatal("TCP listener should be open")
	}
	return srv, tcpAddr
}

// TestTCPListener_LoopbackTrustDefault_DeviceRegistered_NoAuthRequired is
// the server-integration counterpart of
// TestTCPAPIAuth_LoopbackTrustOn_DevicesRegistered_NoCookie_Passes
// (internal/api/auth/api_middleware_test.go) — docs/plans/
// volume-only-daemon.md §論点c: with no explicit config.yaml
// web.loopback_trust key at all, DefaultConfig's default (true) applies, so
// a same-host caller (like the CLI's dedicated TCP(TLS) listener's clients —
// see cliTLSLn) reaches the data API with no Bearer/cookie even once a
// device is already paired — unlike the pre-§論点c bootstrap window, which
// closed the moment the FIRST device registered.
func TestTCPListener_LoopbackTrustDefault_DeviceRegistered_NoAuthRequired(t *testing.T) {
	srv, tcpAddr := newTestServerWithConfig(t, "") // no config.yaml body at all → DefaultConfig()

	store := auth.NewStore(srv.DB())
	if err := store.InsertDevice(context.Background(), "dev-loopback-trust", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	resp, err := http.Get("http://" + tcpAddr + "/api/tasks")
	if err != nil {
		t.Fatalf("tcp loopback GET /api/tasks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("tcp loopback (device registered, loopback_trust default true) GET /api/tasks: got 401, want loopback-trust pass")
	}
}

// TestTCPListener_LoopbackTrustFalse_DeviceRegistered_StillGated pins the
// operator opt-out: `web.loopback_trust: false` restores the pre-§論点c
// bootstrap-only behavior even for a genuine loopback caller.
func TestTCPListener_LoopbackTrustFalse_DeviceRegistered_StillGated(t *testing.T) {
	srv, tcpAddr := newTestServerWithConfig(t, "web:\n  loopback_trust: false\n")

	store := auth.NewStore(srv.DB())
	if err := store.InsertDevice(context.Background(), "dev-strict", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	resp, err := http.Get("http://" + tcpAddr + "/api/tasks")
	if err != nil {
		t.Fatalf("tcp loopback GET /api/tasks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tcp loopback (device registered, loopback_trust=false) GET /api/tasks: status = %d, want 401", resp.StatusCode)
	}
}
