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
