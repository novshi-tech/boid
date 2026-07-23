package server_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/server"
)

// TestServer_Start_ContainerBackend_GatewayURLUsesComposeServiceDNS pins
// [Blocker 2, PR7 codex review] end to end, through the real Server.New +
// Start wiring (not just the pure gatewayURLFor/gatewayBindHost helpers —
// see wire_gateway_url_routing_test.go for those): with
// `sandbox.backend: container` configured, Server.GatewayURL() must be the
// compose-service-DNS + mTLS URL (https://boid-gateway:<tlsPort>), not the
// userns-only http://10.0.2.2:<port> loopback projection a docker sibling
// container has no route to at all — and the TLS listener itself must be
// bound on a non-loopback-only address (composeBindHost, "0.0.0.0") so a
// sibling container can actually reach it.
//
// client.New(client.FromEnv) (internal/server/wire.go's
// sandboxBackendForConfig) does not dial docker eagerly, so this needs no
// live docker daemon — same as TestSandboxBackendForConfig_Container_
// ReturnsContainerBackend in the internal (whitebox) package.
func TestServer_Start_ContainerBackend_GatewayURLUsesComposeServiceDNS(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	boidConfigDir := filepath.Join(configHome, "boid")
	if err := os.MkdirAll(boidConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidConfigDir, "config.yaml"), []byte("sandbox:\n  backend: container\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

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

// TestServer_Start_UsernsBackend_GatewayURLUnaffected pins the companion
// non-regression: no sandbox.backend configured (the default, every
// pre-PR7 deployment) must leave GatewayURL/GatewayTLSAddr byte-for-byte
// unchanged — the plaintext loopback projection and a loopback-bound TLS
// listener, exactly as before this fix.
func TestServer_Start_UsernsBackend_GatewayURLUnaffected(t *testing.T) {
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

	gatewayURL := srv.GatewayURL()
	if !strings.HasPrefix(gatewayURL, "http://10.0.2.2:") {
		t.Errorf("GatewayURL() = %q, want the unchanged http://10.0.2.2:<port> literal", gatewayURL)
	}

	tlsAddr := srv.GatewayTLSAddr()
	if tlsAddr == "" {
		t.Fatal("GatewayTLSAddr() is empty, want a bound TLS listener")
	}
	if !strings.HasPrefix(tlsAddr, "127.0.0.1:") {
		t.Errorf("GatewayTLSAddr() = %q, want it still bound on 127.0.0.1 (userns backend — unchanged)", tlsAddr)
	}
}
