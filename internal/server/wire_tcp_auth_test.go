package server_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

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
