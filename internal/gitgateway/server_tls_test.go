package gitgateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/mtls"
)

// TestGitGatewayTCPListener_MutualTLSHandshake pins
// docs/plans/phase6-container-backend.md §PR4: the git gateway gains a
// TCP(mTLS) listener (Server.ListenTLS) additive to the plaintext loopback
// listener internal/server.Server already binds for the userns backend. A
// client presenting a certificate signed by the gateway's CA completes the
// mTLS handshake and reaches the same ServeHTTP routing/authorization
// logic every other gateway test exercises over plain HTTP.
func TestGitGatewayTCPListener_MutualTLSHandshake(t *testing.T) {
	// No registered routes needed: a well-formed-but-unauthorized request
	// reaching ServeHTTP (rather than failing at the transport layer) is
	// sufficient proof the mTLS handshake and HTTP layer both worked.
	gw := NewServer(NewRegistry(), nil, nil)

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	serverTLSConfig, err := ca.ServerTLSConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	ln, err := gw.ListenTLS("127.0.0.1:0", serverTLSConfig)
	if err != nil {
		t.Fatalf("ListenTLS: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	clientCert, err := ca.IssueClientCert("test-sandbox")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	clientTLSConfig := ca.ClientTLSConfig("127.0.0.1", clientCert)

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientTLSConfig},
	}
	resp, err := client.Get("https://" + ln.Addr().String() + "/j/bogus-token/example.com/owner/repo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET over mTLS: %v", err)
	}
	defer resp.Body.Close()

	// An invalid token is expected to be rejected at the application
	// layer (401) — the point of this test is that we got an HTTP
	// response at all, proving the mTLS handshake succeeded.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (invalid token, but a valid HTTP round trip)", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestGitGatewayTCPListener_RejectsClientWithoutCertificate pins the "無い
// 接続は拒否する" skeleton-mTLS requirement (§PR4): a TLS client with no
// certificate must not reach ServeHTTP at all.
func TestGitGatewayTCPListener_RejectsClientWithoutCertificate(t *testing.T) {
	gw := NewServer(NewRegistry(), nil, nil)

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	serverTLSConfig, err := ca.ServerTLSConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	ln, err := gw.ListenTLS("127.0.0.1:0", serverTLSConfig)
	if err != nil {
		t.Fatalf("ListenTLS: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	_, err = client.Get("https://" + ln.Addr().String() + "/j/bogus-token/example.com/owner/repo.git/info/refs?service=git-upload-pack")
	if err == nil {
		t.Fatal("GET without a client certificate succeeded; want a ClientAuth rejection")
	}
}

// TestGitGatewayTCPListener_CloseTLSClosesKeepAliveConnections pins the fix
// for codex review finding [Minor 4] on PR4: CloseTLS (which
// internal/server.Server.Stop now calls instead of bare-closing the
// net.Listener) tears down an already-accepted keep-alive connection, not
// just the Accept loop that produces new ones.
func TestGitGatewayTCPListener_CloseTLSClosesKeepAliveConnections(t *testing.T) {
	gw := NewServer(NewRegistry(), nil, nil)

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	serverTLSConfig, err := ca.ServerTLSConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	ln, err := gw.ListenTLS("127.0.0.1:0", serverTLSConfig)
	if err != nil {
		t.Fatalf("ListenTLS: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	clientCert, err := ca.IssueClientCert("test-sandbox")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	clientTLSConfig := ca.ClientTLSConfig("127.0.0.1", clientCert)

	// Dial the raw connection ourselves (rather than via http.Client) so
	// the test observes what happens to *this exact* TCP connection —
	// http.Client's Transport would otherwise silently retry an idempotent
	// GET on a fresh connection if the old one turned out to be closed,
	// masking exactly the failure mode this test exists to catch.
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLSConfig)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()

	url := "https://" + ln.Addr().String() + "/j/bogus-token/example.com/owner/repo.git/info/refs?service=git-upload-pack"
	req1, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request 1: %v", err)
	}
	if err := req1.Write(conn); err != nil {
		t.Fatalf("write request 1: %v", err)
	}
	resp1, err := http.ReadResponse(bufio.NewReader(conn), req1)
	if err != nil {
		t.Fatalf("read response 1: %v", err)
	}
	// Drain and close the body so the connection settles into the idle
	// keep-alive state (rather than being left mid-response) before
	// CloseTLS runs — that idle state is exactly what a bare
	// net.Listener.Close() fails to tear down.
	if _, err := io.Copy(io.Discard, resp1.Body); err != nil {
		t.Fatalf("drain response 1 body: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status 1 = %d, want %d (invalid token, but a valid HTTP round trip)", resp1.StatusCode, http.StatusUnauthorized)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := gw.CloseTLS(ctx); err != nil {
		t.Fatalf("CloseTLS: %v", err)
	}

	// The connection was idle when CloseTLS ran; it must now be closed
	// rather than still willing to serve a second request.
	req2, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request 2: %v", err)
	}
	_ = req2.Write(conn) // may itself fail (broken pipe); either way the read below must fail
	if resp2, err := http.ReadResponse(bufio.NewReader(conn), req2); err == nil {
		resp2.Body.Close()
		t.Fatal("read a response for request 2 on a connection CloseTLS should have closed")
	}
}
