package gitgateway

import (
	"crypto/tls"
	"net/http"
	"testing"

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
