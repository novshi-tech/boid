package sandbox_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// sendCONNECT sends a raw HTTP CONNECT request to the proxy and returns the response.
// A deadline is set on the connection to avoid hanging in test environments.
func sendCONNECT(proxyAddr, targetHost string, timeout time.Duration) (*http.Response, net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("dial proxy: %w", err)
	}
	conn.SetDeadline(time.Now().Add(timeout))

	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetHost, targetHost)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("write CONNECT: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	return resp, conn, nil
}

func TestProxy_AllowedDomain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a local TCP server to act as the target so we don't need external network
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer targetLn.Close()
	go func() {
		for {
			conn, err := targetLn.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	targetAddr := targetLn.Addr().String()
	targetHost, targetPort, _ := net.SplitHostPort(targetAddr)
	_ = targetHost

	// Use 127.0.0.1 as allowed domain so the proxy can actually connect
	proxy := sandbox.NewProxy([]string{"127.0.0.1"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)
	connectTarget := fmt.Sprintf("127.0.0.1:%s", targetPort)
	resp, conn, err := sendCONNECT(proxyAddr, connectTarget, 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("allowed domain status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestProxy_BlockedDomain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxy := sandbox.NewProxy([]string{"allowed.com"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)
	resp, conn, err := sendCONNECT(proxyAddr, "blocked.com:443", 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("blocked domain status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestProxy_DotlessTarget_RefusedWith502 pins the §⓪-b fix
// (docs/plans/phase6-cutover-followups.md): a CONNECT for a single-label
// hostname that is not one of the boid-infrastructure aliases must be
// refused with 502 Bad Gateway BEFORE the allowlist check runs, so a
// misconfigured allowed_domains (e.g. one that names another workspace's
// sibling by bare hostname) cannot let a workspace A job reach a workspace
// B sibling the shared-daemon-container's docker embedded DNS happens to
// know how to resolve. Placed ahead of the isDomainAllowed call, so an
// operator adding "sib-b" to allowed_domains does not accidentally
// re-open the cross-workspace hole.
func TestProxy_DotlessTarget_RefusedWith502(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Give the allowlist the bare-hostname target explicitly, so any regression
	// (e.g. the dotless check accidentally being moved AFTER the allowlist
	// check, or removed altogether) would let this CONNECT succeed and fail
	// the assertion below — that is the exact regression this test guards.
	proxy := sandbox.NewProxy([]string{"sib-b"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)
	resp, conn, err := sendCONNECT(proxyAddr, "sib-b:8080", 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("dotless target status = %d, want %d (Bad Gateway) — allowlist must not take precedence over the §⓪-b dotless refusal",
			resp.StatusCode, http.StatusBadGateway)
	}
}

// TestProxy_DottedTarget_RunsAllowlist confirms the §⓪-b fix does not
// affect the normal allowlist path for real dotted hostnames — the same
// listener that refuses "sib-b" above with 502 must still refuse an
// unlisted "example.com" with 403 (the isDomainAllowed default), not 502.
// A regression that moved the dotless check to also match dotted names
// would surface here.
func TestProxy_DottedTarget_RunsAllowlist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxy := sandbox.NewProxy([]string{"allowed.com"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)
	resp, conn, err := sendCONNECT(proxyAddr, "example.com:443", 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("dotted unlisted target status = %d, want %d (Forbidden from allowlist, not %d from dotless refusal)",
			resp.StatusCode, http.StatusForbidden, http.StatusBadGateway)
	}
}

// TestProxy_DotlessBoidInfraName_FallsThroughToAllowlist confirms the boid-
// infrastructure allowlist inside isRefusedDotlessTarget (localhost,
// boid-broker, boid-gateway, boid-egress) genuinely bypasses the dotless
// refusal — those names never actually hit the proxy in practice (they are
// carried in no_proxy at dispatch time) but the defense-in-depth exemption
// must exist so a misconfigured no_proxy does not silently break broker
// RPC or gitgateway clones.
func TestProxy_DotlessBoidInfraName_FallsThroughToAllowlist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxy := sandbox.NewProxy(nil) // empty allowlist — expect 403 (allowlist), not 502 (dotless)
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	for _, target := range []string{"localhost:80", "boid-broker:9000", "boid-gateway:8080", "boid-egress:8080"} {
		proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)
		resp, conn, err := sendCONNECT(proxyAddr, target, 5*time.Second)
		if err != nil {
			t.Fatalf("sendCONNECT(%q): %v", target, err)
		}
		conn.Close()
		if resp.StatusCode == http.StatusBadGateway {
			t.Errorf("boid infra target %q was refused with 502 — must fall through to the allowlist path (403 in this test's empty-allowlist setup)", target)
		}
	}
}

// TestProxy_HTTPForward_DotlessRefused pins the §⓪-b fix's HTTP-forward
// path counterpart to TestProxy_DotlessTarget_RefusedWith502's CONNECT
// path — both dispatch chains (handleConnect and handleHTTP) must refuse
// dotless targets, not only the CONNECT one; a plaintext http:// GET
// through HTTP_PROXY hits handleHTTP, not handleConnect.
func TestProxy_HTTPForward_DotlessRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxy := sandbox.NewProxy([]string{"sib-b"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		return url.Parse(proxyURL)
	}}}

	resp, err := client.Get("http://sib-b:8080/some/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("dotless HTTP forward status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestProxy_SuffixDomainMatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a local TCP target so we don't depend on DNS resolution
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer targetLn.Close()
	go func() {
		for {
			conn, err := targetLn.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	_, targetPort, _ := net.SplitHostPort(targetLn.Addr().String())

	// ".0.0.1" suffix matches "127.0.0.1"
	proxy := sandbox.NewProxy([]string{".0.0.1"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)

	// 127.0.0.1 should match .0.0.1 suffix
	resp, conn, err := sendCONNECT(proxyAddr, fmt.Sprintf("127.0.0.1:%s", targetPort), 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT: %v", err)
	}
	conn.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Error("127.0.0.1 should match .0.0.1 suffix")
	}

	// 192.168.1.2 should be blocked (doesn't match .0.0.1)
	resp2, conn2, err := sendCONNECT(proxyAddr, "192.168.1.2:443", 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT: %v", err)
	}
	conn2.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("192.168.1.2 should be blocked, got %d", resp2.StatusCode)
	}
}

func TestProxy_HTTPForward_Blocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxy := sandbox.NewProxy([]string{"allowed.com"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		return url.Parse(proxyURL)
	}}}

	resp, err := client.Get("http://blocked.com/test")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("blocked domain HTTP forward status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestProxy_SetAllowed_LiveUpdate(t *testing.T) {
	// Verify that SetAllowed swaps the allowlist of an already-listening
	// proxy: a previously-blocked domain becomes allowed (and vice versa)
	// without restarting the listener.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Local TCP target so CONNECT can complete.
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer targetLn.Close()
	go func() {
		for {
			conn, err := targetLn.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	_, targetPort, _ := net.SplitHostPort(targetLn.Addr().String())
	connectTarget := fmt.Sprintf("127.0.0.1:%s", targetPort)

	// Initially block 127.0.0.1.
	proxy := sandbox.NewProxy([]string{"allowed.com"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)

	resp, conn, err := sendCONNECT(proxyAddr, connectTarget, 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT (before): %v", err)
	}
	conn.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("before SetAllowed: status = %d, want 403", resp.StatusCode)
	}

	// Live-swap the allowlist to include 127.0.0.1.
	proxy.SetAllowed([]string{"127.0.0.1"})

	resp2, conn2, err := sendCONNECT(proxyAddr, connectTarget, 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT (after): %v", err)
	}
	defer conn2.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("after SetAllowed: status = %d, want 200", resp2.StatusCode)
	}

	// And swap it back away.
	proxy.SetAllowed([]string{"only.this.one"})
	resp3, conn3, err := sendCONNECT(proxyAddr, connectTarget, 5*time.Second)
	if err != nil {
		t.Fatalf("sendCONNECT (after revoke): %v", err)
	}
	conn3.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Errorf("after revoke: status = %d, want 403", resp3.StatusCode)
	}
}

func TestProxy_SetAllowed_RaceSafe(t *testing.T) {
	// Hammer SetAllowed and isDomainAllowed concurrently to surface any
	// data race under `go test -race`. The test is purely about
	// concurrency safety; correctness of the allow/deny decision is
	// covered by the other tests in this file.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxy := sandbox.NewProxy([]string{"a.example.com", "b.example.com"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			proxy.SetAllowed([]string{"a.example.com", fmt.Sprintf("d%d.example.com", i)})
		}
	}()
	for i := 0; i < 50; i++ {
		resp, conn, err := sendCONNECT(proxyAddr, "a.example.com:443", 2*time.Second)
		if err != nil {
			t.Fatalf("sendCONNECT[%d]: %v", i, err)
		}
		conn.Close()
		// Status should always be one of the two well-defined responses.
		if resp.StatusCode != http.StatusOK &&
			resp.StatusCode != http.StatusForbidden &&
			resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("unexpected status %d", resp.StatusCode)
		}
	}
	<-done
}

func TestProxy_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	proxy := sandbox.NewProxy([]string{"example.com"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)

	// Verify proxy is listening by making a request (blocked is fine, just need a response)
	resp, conn, err := sendCONNECT(proxyAddr, "blocked.com:443", 5*time.Second)
	if err != nil {
		t.Fatalf("initial request: %v", err)
	}
	conn.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Logf("unexpected initial status: %d", resp.StatusCode)
	}

	// Cancel context to stop proxy
	cancel()
	time.Sleep(100 * time.Millisecond)

	// Verify proxy no longer accepts connections
	_, _, err = sendCONNECT(proxyAddr, "example.com:443", 2*time.Second)
	if err == nil {
		t.Error("expected error after context cancellation, proxy should be stopped")
	}
}

// --- Append the following test function to proxy_test.go ---

func TestProxy_ConnectResponseFormat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a local TCP echo server as the tunnel target
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer targetLn.Close()
	go func() {
		for {
			conn, err := targetLn.Accept()
			if err != nil {
				return
			}
			// Echo back what we receive
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				c.Write(buf[:n])
			}(conn)
		}
	}()
	_, targetPort, _ := net.SplitHostPort(targetLn.Addr().String())

	proxy := sandbox.NewProxy([]string{"127.0.0.1"})
	port, err := proxy.Start(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Stop()

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", port)
	connectTarget := fmt.Sprintf("127.0.0.1:%s", targetPort)

	// Send raw CONNECT and inspect the response
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", connectTarget, connectTarget)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify no Content-Length header (raw tunnel, not HTTP body)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		t.Errorf("CONNECT response should not have Content-Length header, got %q", cl)
	}

	// Verify tunnel works: send data through and get it echoed back
	testData := "hello tunnel"
	if _, err := conn.Write([]byte(testData)); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}

	buf := make([]byte, len(testData))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read from tunnel: %v", err)
	}
	if string(buf[:n]) != testData {
		t.Errorf("tunnel echo = %q, want %q", string(buf[:n]), testData)
	}
}
