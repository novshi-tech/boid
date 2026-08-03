package server_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/server"
)

// TestServer_SharedGatewayListener_RoutesBothGatewaysOverTLS is the
// end-to-end pin for docs/plans/api-gateway.md 論点1 ("同居 — path prefix
// /j/ と /api/ で分岐。mTLS listener も流用"): a real daemon (server.New +
// Start, the exact production wiring path) must serve BOTH the git gateway
// and the API gateway off the SAME TCP(mTLS) listener — internal/server's
// combined-handler wiring (wire.go's combinedGatewayHandler construction,
// Start's TLS block reading srv.combinedGatewayHandler instead of calling
// gitgateway.Server.ListenTLS) is the only thing this test exercises;
// each gateway's own request-handling correctness is covered exhaustively
// by its own package's tests.
func TestServer_SharedGatewayListener_RoutesBothGatewaysOverTLS(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	srv, err := server.New(server.Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		Backend:    &noopBackend{},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	caPEM := srv.GatewayCAPEM()
	if len(caPEM) == 0 {
		t.Fatal("GatewayCAPEM() is empty, want the daemon CA's own PEM cert once Start has run with TLSDir set")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("AppendCertsFromPEM: GatewayCAPEM() did not parse as a valid PEM cert")
	}

	addr := srv.GatewayTLSAddr()
	if addr == "" {
		t.Fatal("GatewayTLSAddr() is empty, want a bound TLS listener")
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

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{
			name:       "git gateway route reaches gitgateway.Server (unknown token -> 401)",
			path:       gitgateway.PathPrefix + "bogus-token/github.com/owner/repo/info/refs?service=git-upload-pack",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "API gateway route reaches apigateway.Server (unknown token -> 401)",
			path:       apigateway.PathPrefix + "bogus-token/myapp/v1/users",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unmatched path 404s on the shared listener",
			path:       "/healthz",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := client.Get("https://" + addr + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Errorf("GET %s: status = %d, want %d", c.path, resp.StatusCode, c.wantStatus)
			}
		})
	}
}
