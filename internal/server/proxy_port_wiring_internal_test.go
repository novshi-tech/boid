package server

import (
	"path/filepath"
	"testing"
)

// TestNew_WiresEgressProxyPortStore pins the one seam the rest of the
// stable-port feature's tests do not cover: config.yaml -> server.Config is
// tested in cmd, and ProxyManager's allocation behaviour is tested in
// internal/sandbox, but nothing asserted that New() actually JOINS them.
//
// Without this, deleting the three assignments in New() leaves every test in
// the feature passing while every egress proxy silently returns to ephemeral
// ports — precisely the regression the feature exists to prevent, and
// invisible to the suite (docs/plans/egress-proxy-stable-port.md).
func TestNew_WiresEgressProxyPortStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, err := New(Config{
		DBPath:              ":memory:",
		SocketPath:          filepath.Join(t.TempDir(), "boid.sock"),
		HTTPAddr:            "127.0.0.1:0",
		CLIAddr:             "127.0.0.1:0",
		EgressProxyPortLow:  20000,
		EgressProxyPortHigh: 20999,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if srv.proxyManager == nil {
		t.Fatal("proxyManager is nil")
	}
	if srv.proxyManager.PortStore == nil {
		t.Error("proxyManager.PortStore is nil — egress proxy ports would go back to being ephemeral, silently")
	}
	if srv.proxyManager.PortRangeLow != 20000 || srv.proxyManager.PortRangeHigh != 20999 {
		t.Errorf("proxyManager band = [%d, %d], want the configured [20000, 20999]",
			srv.proxyManager.PortRangeLow, srv.proxyManager.PortRangeHigh)
	}
}
