package server_test

import (
	"context"
	"crypto/tls"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/server"
)

// TestServer_Start_TLSListenerCertsIncludeComposeServiceNames pins the fix
// for codex review finding [Major 2] on PR4: the broker and git gateway
// TCP(mTLS) listener certs that Server.Start issues must carry their
// compose service DNS names as SANs, in addition to the loopback names
// every PR4 caller actually dials today — otherwise a PR5+ container
// backend caller reaching them by service name would fail hostname
// verification.
func TestServer_Start_TLSListenerCertsIncludeComposeServiceNames(t *testing.T) {
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

	// Start() (via mtls.LoadOrCreate) generated the CA under tlsDir; load
	// it again here (§PR4's own "既存を再利用" guarantee) to issue a client
	// cert that can dial the listeners Start bound.
	ca, err := mtls.LoadOrCreate(tlsDir)
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	clientCert, err := ca.IssueClientCert("test-client")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	clientTLSConfig := ca.ClientTLSConfig("127.0.0.1", clientCert)

	cases := []struct {
		name string
		addr string
		want string
	}{
		{"broker", srv.BrokerTLSAddr(), "boid-broker"},
		{"gateway", srv.GatewayTLSAddr(), "boid-gateway"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.addr == "" {
				t.Fatalf("%s TLS listener address is empty", tc.name)
			}

			conn, err := tls.Dial("tcp", tc.addr, clientTLSConfig)
			if err != nil {
				t.Fatalf("tls.Dial %s: %v", tc.name, err)
			}
			defer conn.Close()

			state := conn.ConnectionState()
			if len(state.PeerCertificates) == 0 {
				t.Fatal("no server certificate observed on the connection state")
			}
			dnsNames := state.PeerCertificates[0].DNSNames
			found := false
			for _, n := range dnsNames {
				if n == tc.want {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s cert DNSNames = %v, want %q included", tc.name, dnsNames, tc.want)
			}
		})
	}
}
