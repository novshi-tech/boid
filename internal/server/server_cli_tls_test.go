package server_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/server"
)

// TestServer_CLIAddr_Empty_NoListenerBound pins the default-off contract for
// Config.CLIAddr (docs/plans/volume-only-daemon.md §論点c): a *Server built
// without CLIAddr set — every pre-§論点c caller/test, and any future one
// that does not care about this listener — binds no CLI TLS listener at
// all, even with a TLS CA otherwise configured.
func TestServer_CLIAddr_Empty_NoListenerBound(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	srv, err := server.New(server.Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		JobRuntime: noopRuntime{},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if addr := srv.CLITLSAddr(); addr != "" {
		t.Fatalf("CLITLSAddr() = %q, want empty (CLIAddr was not set)", addr)
	}
}

// newCLITLSTestServer boots a *server.Server with both TLSDir and CLIAddr
// set — the standard shape a real `boid start` (cmd/start.go) always
// produces — and returns it alongside the bound CLI TLS listener's address.
func newCLITLSTestServer(t *testing.T) (srv *server.Server, cliAddr string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	srv, err := server.New(server.Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		CLIAddr:    "127.0.0.1:0",
		JobRuntime: noopRuntime{},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	cliAddr = srv.CLITLSAddr()
	if cliAddr == "" {
		t.Fatal("CLITLSAddr() is empty, want a bound TLS listener (CLIAddr was set)")
	}
	return srv, cliAddr
}

// TestServer_CLITLS_DistinctFromWebUIPort pins the plan's own port-
// separation decision (docs/plans/volume-only-daemon.md §論点c: "separate
// from Web UI's 8080... pick a distinct port") at the wiring level: the CLI
// TLS listener and the Web UI's own TCP listener are two independently
// bound sockets, not the same one wearing two names.
func TestServer_CLITLS_DistinctFromWebUIPort(t *testing.T) {
	srv, cliAddr := newCLITLSTestServer(t)

	webAddr := srv.TCPAddr()
	if webAddr == "" {
		t.Fatal("TCPAddr() is empty, want the Web UI's own TCP listener bound")
	}
	if webAddr == cliAddr {
		t.Fatalf("CLITLSAddr() (%s) == TCPAddr() (%s), want two distinct listeners", cliAddr, webAddr)
	}
}

// TestServer_CLITLS_RequiresServerCATrust pins that this listener really is
// TLS, not plaintext: a client that does not trust the daemon's internal CA
// fails the handshake outright (default Go system root pool rejects a
// self-signed cert).
func TestServer_CLITLS_RequiresServerCATrust(t *testing.T) {
	_, cliAddr := newCLITLSTestServer(t)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := client.Get("https://" + cliAddr + "/api/health")
	if err == nil {
		t.Fatal("GET against the CLI TLS listener with an untrusted client succeeded, want a certificate verification failure")
	}
}

// TestServer_CLITLS_NoClientCert_LoopbackTrust_ReachesGatedAPI is the core
// end-to-end pin for docs/plans/volume-only-daemon.md §論点c/§決定 (loopback
// trust default ON): a client that trusts the daemon's own CA (the
// "boid print-cli-profile" bootstrap flow's whole point — see cmd/start.go)
// but presents NO client certificate and NO Bearer/cookie can still reach a
// gated /api/* route, because (1) this listener uses ServerOnlyTLSConfig —
// no client cert is required at the TLS layer at all — and (2) the shared
// tcpHandler's auth.NewTCPAPIAuthMiddleware sees a genuinely loopback
// RemoteAddr (this test dials 127.0.0.1) and — with web.loopback_trust
// defaulting to true — lets the request through with no Bearer/cookie
// either. This is what makes `boid task list` work out of the box against
// the compose daemon with zero `boid login`/device-pair ceremony.
func TestServer_CLITLS_NoClientCert_LoopbackTrust_ReachesGatedAPI(t *testing.T) {
	srv, cliAddr := newCLITLSTestServer(t)

	caPEM := srv.GatewayCAPEM()
	if len(caPEM) == 0 {
		t.Fatal("GatewayCAPEM() is empty, want the daemon CA's own PEM cert once Start has run with TLSDir set")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("AppendCertsFromPEM: GatewayCAPEM() did not parse as a valid PEM cert")
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "127.0.0.1",
			},
		},
	}
	// /api/tasks is a real gated API route (see wire_tcp_auth_test.go's
	// TestTCPListener_DataAPI_RequiresAuth) — /api/health would pass
	// regardless of auth and would not distinguish this from a broken
	// listener.
	resp, err := client.Get("https://" + cliAddr + "/api/tasks")
	if err != nil {
		t.Fatalf("GET /api/tasks with no client cert, no Bearer, no cookie: %v (want a real HTTP response — loopback trust should authorize this)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("GET /api/tasks: status = %d, want != 401 (loopback trust should authorize a same-host caller with no credentials)", resp.StatusCode)
	}
}
