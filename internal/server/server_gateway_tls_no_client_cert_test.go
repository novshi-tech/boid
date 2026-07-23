package server_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/server"
)

// TestServer_Start_GatewayTLS_DoesNotRequireClientCert pins the PR9
// e2e-container CI fix: a sandbox-internal `git clone` against the
// container backend's TLS-secured gateway must be able to complete a real
// HTTPS round trip with NO client certificate at all — the git gateway's
// own per-job Registry token (URL-path-embedded, verified by
// gatewayHandler.ServeHTTP) is the actual per-job authorization mechanism,
// not TLS client identity (see mtls.CA.ServerOnlyTLSConfig's own doc
// comment). Before this fix, Server.Start wired the gateway's TLS listener
// with ServerTLSConfig (unconditional tls.RequireAndVerifyClientCert),
// which no real client (a sandbox's plain `git`) ever satisfied — the
// real-docker e2e-container CI job's every sandbox-internal clone attempt
// failed the handshake outright: "tls: client didn't provide a
// certificate" server-side.
//
// A full http.Client round trip (not a bare tls.Dial) is required to
// actually observe this: TLS 1.3's client-certificate check happens on the
// SERVER after it has already sent its own Finished message, so a client's
// tls.Dial can return successfully even against a listener that will go on
// to reject the connection — the rejection alert only arrives on a
// subsequent read. An HTTP GET forces exactly that round trip.
//
// srv.GatewayCAPEM() is the client-side half of this fix: a real client
// (unlike an insecure-skip-verify test double) must also trust the
// server's certificate, exactly as SandboxRuntimeInfo.GatewayCAPEM
// delivers to a real sandbox (as a file, GIT_SSL_CAINFO-pointed) — see
// sandbox_builder.go's own call site.
func TestServer_Start_GatewayTLS_DoesNotRequireClientCert(t *testing.T) {
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

	// No Certificates set at all — this is what a real sandbox's `git`
	// process does (GIT_SSL_CAINFO trusts the server; git presents no
	// client cert of its own, since it has none).
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "127.0.0.1",
			},
		},
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("GET with no client cert: %v (want a real HTTP response — the gateway's own per-job token is the authorization mechanism, not TLS client identity)", err)
	}
	resp.Body.Close()
}

// TestServer_Start_BrokerTLS_StillRequiresClientCert is the companion
// non-regression pin: this fix touches ONLY the git gateway's own TLS
// config (internal/server/server.go's gitgateway ListenTLS call site) —
// the broker's separate TCP(mTLS) listener must still reject a connection
// with no client certificate, exactly as ServerTLSConfig (mutual TLS)
// already did before this fix. See the gateway test's own doc comment for
// why a full HTTP round trip (not a bare tls.Dial) is required to actually
// observe a TLS 1.3 client-cert rejection.
func TestServer_Start_BrokerTLS_StillRequiresClientCert(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	tlsDir := filepath.Join(tmpDir, "tls")
	srv, err := server.New(server.Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     tlsDir,
		JobRuntime: noopRuntime{},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	ca, err := mtls.LoadOrCreate(tlsDir)
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	pool := ca.CertPool()

	addr := srv.BrokerTLSAddr()
	if addr == "" {
		t.Fatal("BrokerTLSAddr() is empty, want a bound TLS listener")
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
	resp, err := client.Get("https://" + addr + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("GET with no client cert against the broker TLS listener succeeded, want a handshake/connection failure (broker's mTLS requirement is unchanged by this fix)")
	}
}
