package dockerproxy

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/mtls"
)

// composeDockerProxyServiceName is the compose network DNS name a PR5+
// container-backend caller will dial the docker proxy's TCP(mTLS)
// listener by (codex review [Major 2] on
// docs/plans/phase6-container-backend.md §PR4). No production caller
// wires dockerproxy's TLS listener in PR4 (see ServeTLS's own doc
// comment) — this test is the only site issuing a cert for it today — but
// the SAN needs pinning here so a future caller inherits it rather than
// rediscovering the same hostname-verification failure the broker/gateway
// listeners had (see internal/server.Server's composeBrokerServiceName /
// composeGatewayServiceName).
const composeDockerProxyServiceName = "boid-dockerproxy"

// TestDockerProxyTCPListener_MutualTLSHandshake pins
// docs/plans/phase6-container-backend.md §PR4: the docker proxy gains a
// TCP(mTLS) counterpart to its existing per-sandbox UNIX socket transport
// (ServeTLS wraps a plain net.Listener with a client-cert-requiring
// tls.Config and dispatches through the exact same serveHTTP / policy
// allowlist as Serve). A client presenting a certificate signed by the
// proxy's CA completes the mTLS handshake and gets a normal proxied
// response over TCP.
func TestDockerProxyTCPListener_MutualTLSHandshake(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Version":"test"}`))
	})

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	serverTLSConfig, err := ca.ServerTLSConfig("127.0.0.1", composeDockerProxyServiceName)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}

	proxy := New(upstream.sockPath)
	go proxy.ServeTLS(tcpLn, serverTLSConfig) //nolint:errcheck
	t.Cleanup(func() { proxy.Close() })

	clientCert, err := ca.IssueClientCert("test-job")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	clientTLSConfig := ca.ClientTLSConfig("127.0.0.1", clientCert)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: clientTLSConfig,
		},
	}
	resp, err := client.Get("https://" + tcpLn.Addr().String() + "/version")
	if err != nil {
		t.Fatalf("GET over mTLS: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != `{"Version":"test"}` {
		t.Fatalf("body = %q, want the upstream's response verbatim", body)
	}
}

// TestDockerProxyTCPListener_RejectsClientWithoutCertificate pins the
// "無い接続は拒否する" skeleton-mTLS requirement (§PR4): a TLS client with no
// certificate must not reach the proxy's policy allowlist / upstream.
func TestDockerProxyTCPListener_RejectsClientWithoutCertificate(t *testing.T) {
	var upstreamHit bool
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.WriteHeader(http.StatusOK)
	})

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	serverTLSConfig, err := ca.ServerTLSConfig("127.0.0.1", composeDockerProxyServiceName)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}

	proxy := New(upstream.sockPath)
	go proxy.ServeTLS(tcpLn, serverTLSConfig) //nolint:errcheck
	t.Cleanup(func() { proxy.Close() })

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	_, err = client.Get("https://" + tcpLn.Addr().String() + "/version")
	if err == nil {
		t.Fatal("GET without a client certificate succeeded; want a ClientAuth rejection")
	}
	if upstreamHit {
		t.Fatal("request reached the upstream despite a missing client certificate")
	}
}

// TestDockerProxyTCPListener_CertIncludesComposeServiceName pins the fix
// for codex review finding [Major 2] on PR4: the server cert the docker
// proxy's TLS listener presents must carry the compose service DNS name a
// PR5+ container-backend caller will dial it by, not just the loopback
// names PR4's own tests use — otherwise that future caller's hostname
// verification would fail.
func TestDockerProxyTCPListener_CertIncludesComposeServiceName(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	serverTLSConfig, err := ca.ServerTLSConfig("127.0.0.1", composeDockerProxyServiceName)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}

	proxy := New(upstream.sockPath)
	go proxy.ServeTLS(tcpLn, serverTLSConfig) //nolint:errcheck
	t.Cleanup(func() { proxy.Close() })

	clientCert, err := ca.IssueClientCert("test-job")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	clientTLSConfig := ca.ClientTLSConfig("127.0.0.1", clientCert)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: clientTLSConfig,
		},
	}
	resp, err := client.Get("https://" + tcpLn.Addr().String() + "/version")
	if err != nil {
		t.Fatalf("GET over mTLS: %v", err)
	}
	defer resp.Body.Close()

	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no server certificate observed on the response's TLS connection state")
	}
	dnsNames := resp.TLS.PeerCertificates[0].DNSNames
	found := false
	for _, name := range dnsNames {
		if name == composeDockerProxyServiceName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("server cert DNSNames = %v, want %q included", dnsNames, composeDockerProxyServiceName)
	}
}
