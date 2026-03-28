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
