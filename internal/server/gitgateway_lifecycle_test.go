package server_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/server"
)

// TestServer_GitGatewayLifecycle is the 4-point-set guard for
// docs/plans/git-gateway-cutover.md PR4: Start must bind the gateway's own
// listener (GatewayURL non-empty, reachable, well-formed
// "http://10.0.2.2:<port>") and Stop must tear it down cleanly (no hang, no
// panic) like every other subserver (broker, proxy manager, UNIX/TCP
// listeners).
func TestServer_GitGatewayLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	cfg := server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Before Start, the gateway listener has not been bound yet.
	if got := srv.GatewayURL(); got != "" {
		t.Fatalf("GatewayURL before Start = %q, want empty", got)
	}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	gwURL := srv.GatewayURL()
	if gwURL == "" {
		t.Fatal("GatewayURL after Start is empty, want http://10.0.2.2:<port>")
	}
	const wantPrefix = "http://10.0.2.2:"
	if len(gwURL) <= len(wantPrefix) || gwURL[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("GatewayURL = %q, want prefix %q", gwURL, wantPrefix)
	}

	// The gateway is inert in PR4 (nothing dispatches through it), but its
	// listener must actually be up and serving — an unrecognized path 404s
	// rather than connection-refuses. GatewayURL uses the sandbox-facing
	// 10.0.2.2 alias, which is unreachable from this test process, so dial
	// the real bound port directly instead (host/port surfaced via the
	// listener, not the sandbox-facing URL).
	client := &http.Client{Timeout: 5 * time.Second}
	// Reconstruct a loopback-reachable URL: same port as GatewayURL, host
	// swapped to 127.0.0.1 (the actual bind address — see Server.Start).
	port := gwURL[len(wantPrefix):]
	resp, err := client.Get("http://127.0.0.1:" + port + "/j/bogus-token/host/owner/repo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET against bound gateway listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (invalid job token, but the listener IS serving gitgateway.Server)", resp.StatusCode)
	}

	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A second connection attempt after Stop must fail (listener closed).
	if _, err := client.Get("http://127.0.0.1:" + port + "/j/bogus-token/host/owner/repo.git/info/refs"); err == nil {
		t.Fatal("expected connection error after Stop, got nil (gateway listener still serving)")
	}
}
