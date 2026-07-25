package server_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/server"
)

// TestServer_Start_ContainerBackend_GatewayURLUsesComposeServiceDNS pins
// [Blocker 2, PR7 codex review] end to end, through the real Server.New +
// Start wiring (not just the pure gatewayURLFor/gatewayBindHost helpers —
// see wire_gateway_url_routing_test.go for those): Server.GatewayURL() must
// be the compose-service-DNS + mTLS URL (https://boid-gateway:<tlsPort>),
// not the userns-only http://10.0.2.2:<port> loopback projection a docker
// sibling container has no route to at all — and the TLS listener itself
// must be bound on a non-loopback-only address (composeBindHost, "0.0.0.0")
// so a sibling container can actually reach it.
//
// PR-4 (docs/plans/volume-only-daemon.md §論点e) made this the ONLY
// behavior — container is the only sandbox backend now, selected
// unconditionally by sandboxBackendForConfig, so this test no longer needs
// to seed a config.yaml or override Config.Backend to get there: a bare
// server.New() already constructs a real containerBackend.
// client.New(client.FromEnv) (internal/server/wire.go's
// sandboxBackendForConfig) does not dial docker eagerly, so this needs no
// live docker daemon — same as TestSandboxBackendForConfig_ReturnsContainerBackend
// in the internal (whitebox) package.
func TestServer_Start_ContainerBackend_GatewayURLUsesComposeServiceDNS(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	srv, err := server.New(server.Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	gatewayURL := srv.GatewayURL()
	if !strings.HasPrefix(gatewayURL, "https://boid-gateway:") {
		t.Errorf("GatewayURL() = %q, want it to start with %q", gatewayURL, "https://boid-gateway:")
	}
	if strings.Contains(gatewayURL, "10.0.2.2") {
		t.Errorf("GatewayURL() = %q, want no trace of the userns-only 10.0.2.2 loopback projection", gatewayURL)
	}

	tlsAddr := srv.GatewayTLSAddr()
	if tlsAddr == "" {
		t.Fatal("GatewayTLSAddr() is empty, want a bound TLS listener")
	}
	// composeBindHost ("0.0.0.0") is what gatewayBindHost passes to
	// net.Listen, but on a dual-stack-enabled host Go's own net package
	// can report the bound address back as "[::]:<port>" instead of
	// "0.0.0.0:<port>" (a well-known net.Listen quirk — the resulting
	// socket is still reachable over IPv4 either way, since Linux's
	// default net.ipv6.bindv6only=0 makes an IPv6 ANY-address listener
	// dual-stack). The functional contract this test actually needs to
	// pin is "NOT loopback-only" — TestGatewayBindHost_Container_
	// ReturnsAllInterfaces (wire_gateway_url_routing_test.go) already pins
	// the exact composeBindHost value gatewayBindHost returns.
	if strings.HasPrefix(tlsAddr, "127.0.0.1:") {
		t.Errorf("GatewayTLSAddr() = %q, want it NOT bound loopback-only — a sibling job container cannot reach a loopback-only listener", tlsAddr)
	}
}
