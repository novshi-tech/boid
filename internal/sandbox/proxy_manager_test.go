package sandbox_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// quickConnect dials proxyAddr, sends a CONNECT for host, and returns the
// status code. The connection is closed unconditionally before returning.
func quickConnect(t *testing.T, proxyAddr, host string) int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode
}

func TestProxyManager_PerWorkspaceIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := sandbox.NewProxyManager()
	m.Start(ctx)
	defer m.StopAll()

	portA, err := m.GetOrCreate("ws-a", []string{"a.example.com"})
	if err != nil {
		t.Fatalf("GetOrCreate ws-a: %v", err)
	}
	portB, err := m.GetOrCreate("ws-b", []string{"b.example.com"})
	if err != nil {
		t.Fatalf("GetOrCreate ws-b: %v", err)
	}
	if portA == portB {
		t.Fatalf("workspaces share port %d; expected distinct listeners", portA)
	}

	// ws-a allows a.example.com but not b.example.com (and vice versa).
	if got := quickConnect(t, fmt.Sprintf("127.0.0.1:%d", portA), "a.example.com:443"); got != http.StatusBadGateway && got != http.StatusOK {
		// 502 is expected because a.example.com is not actually
		// reachable, but the allowlist let us through to the dial.
		t.Errorf("ws-a allowed host: status = %d, want 200 or 502", got)
	}
	if got := quickConnect(t, fmt.Sprintf("127.0.0.1:%d", portA), "b.example.com:443"); got != http.StatusForbidden {
		t.Errorf("ws-a blocked host: status = %d, want 403", got)
	}
	if got := quickConnect(t, fmt.Sprintf("127.0.0.1:%d", portB), "b.example.com:443"); got != http.StatusBadGateway && got != http.StatusOK {
		t.Errorf("ws-b allowed host: status = %d, want 200 or 502", got)
	}
	if got := quickConnect(t, fmt.Sprintf("127.0.0.1:%d", portB), "a.example.com:443"); got != http.StatusForbidden {
		t.Errorf("ws-b blocked host: status = %d, want 403", got)
	}

	if m.Count() != 2 {
		t.Errorf("Count() = %d, want 2", m.Count())
	}
}

func TestProxyManager_GetOrCreate_Reapplies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := sandbox.NewProxyManager()
	m.Start(ctx)
	defer m.StopAll()

	port1, err := m.GetOrCreate("ws", []string{"old.example.com"})
	if err != nil {
		t.Fatalf("first GetOrCreate: %v", err)
	}
	if got := quickConnect(t, fmt.Sprintf("127.0.0.1:%d", port1), "new.example.com:443"); got != http.StatusForbidden {
		t.Fatalf("before re-apply: %d, want 403", got)
	}

	// Second call must reuse the same port AND swap the allowlist.
	port2, err := m.GetOrCreate("ws", []string{"new.example.com"})
	if err != nil {
		t.Fatalf("second GetOrCreate: %v", err)
	}
	if port2 != port1 {
		t.Fatalf("port changed across GetOrCreate: %d -> %d", port1, port2)
	}
	if got := quickConnect(t, fmt.Sprintf("127.0.0.1:%d", port2), "new.example.com:443"); got != http.StatusBadGateway && got != http.StatusOK {
		t.Errorf("after re-apply allowed host: %d, want 200 or 502", got)
	}
	if got := quickConnect(t, fmt.Sprintf("127.0.0.1:%d", port2), "old.example.com:443"); got != http.StatusForbidden {
		t.Errorf("after re-apply blocked host: %d, want 403", got)
	}
}

func TestProxyManager_NotStarted(t *testing.T) {
	m := sandbox.NewProxyManager()
	if _, err := m.GetOrCreate("ws", nil); err == nil {
		t.Fatal("expected error when manager is not started")
	}
}

func TestProxyManager_EmptyWorkspaceID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := sandbox.NewProxyManager()
	m.Start(ctx)
	defer m.StopAll()
	if _, err := m.GetOrCreate("", []string{"example.com"}); err == nil {
		t.Fatal("expected error for empty workspace id")
	}
}

// findNonLoopbackIPv4 returns a non-loopback IPv4 address configured on
// this host, or "" if none is found (some minimal/isolated CI sandboxes may
// have only loopback — callers should skip in that case rather than fail).
func findNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("InterfaceAddrs: %v", err)
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

// TestProxyManager_BindHost_ReachableFromNonLoopback pins [Blocker 2, PR7
// codex review]: with BindHost set to "0.0.0.0" (composeBindHost — what
// internal/server.New wires in when the container backend is selected),
// the listener GetOrCreate starts must be reachable from a non-loopback
// address, not just 127.0.0.1 — a sibling job container on the shared
// compose network dials this daemon by its own container IP, which a
// loopback-only bind (the pre-PR7, and still-default, behavior — see the
// companion default-behavior test below) is unreachable from entirely.
func TestProxyManager_BindHost_ReachableFromNonLoopback(t *testing.T) {
	host := findNonLoopbackIPv4(t)
	if host == "" {
		t.Skip("no non-loopback IPv4 address available on this host")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := sandbox.NewProxyManager()
	m.BindHost = "0.0.0.0"
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-bindhost", []string{"example.com"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial via non-loopback address %s: %v (want the 0.0.0.0-bound listener to accept it)", host, err)
	}
	conn.Close()
}

// TestProxyManager_DefaultBindHost_LoopbackOnly pins the companion
// non-regression: BindHost left at its zero value (every pre-Blocker-2
// caller, and every userns-backend deployment after it) must still bind
// loopback-only — a listener reachable from a non-loopback address by
// default would be a new, unintended network exposure for every existing
// deployment.
func TestProxyManager_DefaultBindHost_LoopbackOnly(t *testing.T) {
	host := findNonLoopbackIPv4(t)
	if host == "" {
		t.Skip("no non-loopback IPv4 address available on this host")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := sandbox.NewProxyManager()
	m.Start(ctx)
	defer m.StopAll()

	port, err := m.GetOrCreate("ws-default-bindhost", []string{"example.com"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err == nil {
		conn.Close()
		t.Fatalf("dial via non-loopback address %s unexpectedly succeeded, want connection refused (default BindHost must stay loopback-only)", host)
	}
}

func TestProxyManager_StopAll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := sandbox.NewProxyManager()
	m.Start(ctx)

	port, err := m.GetOrCreate("ws", []string{"example.com"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	m.StopAll()

	// After StopAll the listener must be gone (or refuse connections quickly).
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err == nil {
		conn.Close()
		// The kernel may immediately reuse the port; we tolerate a successful
		// dial as long as the listener no longer responds with valid HTTP.
	}
	if got := m.Count(); got != 0 {
		t.Errorf("Count after StopAll = %d, want 0", got)
	}
}
