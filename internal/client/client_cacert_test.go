package client

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/mtls"
)

// newSelfSignedTLSListener boots a plain net/http server behind a
// self-signed CA-issued cert (internal/mtls, the same package
// internal/server uses for its dedicated CLI TLS listener — docs/plans/
// volume-only-daemon.md §論点c) and returns its address plus the CA's own
// PEM cert. No httptest.NewTLSServer here: that helper's cert is signed by
// its own throwaway CA baked into the *httptest.Server, which offers no way
// to hand this test a caCertPEM byte slice shaped like what
// NewClientWithCACert actually consumes in production (mtls.CA.CertPEM()).
func newSelfSignedTLSListener(t *testing.T, handler http.Handler) (addr string, caCertPEM []byte) {
	t.Helper()
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	tlsCfg, err := ca.ServerOnlyTLSConfig("127.0.0.1", "localhost")
	if err != nil {
		t.Fatalf("ServerOnlyTLSConfig: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), ca.CertPEM()
}

// TestNewClientWithCACert_TrustsSelfSignedCA is the core pin for
// docs/plans/volume-only-daemon.md §論点c: a client built with
// NewClientWithCACert against a self-signed-CA-backed listener (the exact
// shape internal/server's dedicated CLI TLS listener produces) completes a
// real HTTPS round trip — proving the RootCAs pool actually gets threaded
// through to the transport, not silently ignored.
func TestNewClientWithCACert_TrustsSelfSignedCA(t *testing.T) {
	addr, caCertPEM := newSelfSignedTLSListener(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	c, err := NewClientWithCACert("https://"+addr, "", caCertPEM)
	if err != nil {
		t.Fatalf("NewClientWithCACert: %v", err)
	}

	statusCode, body, err := c.GetRaw("/anything")
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want 200", statusCode)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

// TestNewClientWithCACert_NoCACert_RejectsSelfSignedServer pins the
// negative case: NewClientWithCACert("https://...", token, nil) (equivalent
// to plain NewClient) must NOT trust a self-signed cert just because it
// happens to be reachable — no caCertPEM means "system pool only",
// byte-for-byte pre-§論点c behavior.
func TestNewClientWithCACert_NoCACert_RejectsSelfSignedServer(t *testing.T) {
	addr, _ := newSelfSignedTLSListener(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	c, err := NewClientWithCACert("https://"+addr, "", nil)
	if err != nil {
		t.Fatalf("NewClientWithCACert: %v", err)
	}

	if _, _, err := c.GetRaw("/anything"); err == nil {
		t.Fatal("GetRaw against a self-signed server with no caCertPEM succeeded, want a certificate verification failure")
	}
}

// TestNewClientWithCACert_MalformedPEM_Rejected pins construction-time
// validation: a caCertPEM that is not valid PEM is rejected up front rather
// than silently falling back to "no extra trust" (which would then fail
// with a confusing TLS error far from the actual mistake).
func TestNewClientWithCACert_MalformedPEM_Rejected(t *testing.T) {
	if _, err := NewClientWithCACert("https://example.com", "", []byte("not a pem cert")); err == nil {
		t.Fatal("expected an error for malformed caCertPEM")
	}
}

// TestNewClient_EquivalentToNewClientWithCACertNil pins that NewClient
// itself is now just NewClientWithCACert(url, token, nil) — a refactor
// this PR performs, not a behavior change (net.Listener addresses aside,
// every existing NewClient test in this package continues to pass
// unmodified alongside this file).
func TestNewClient_EquivalentToNewClientWithCACertNil(t *testing.T) {
	c1, err := NewClient("unix:///tmp/x.sock", "tok")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c2, err := NewClientWithCACert("unix:///tmp/x.sock", "tok", nil)
	if err != nil {
		t.Fatalf("NewClientWithCACert: %v", err)
	}
	if c1.socketPath != c2.socketPath {
		t.Errorf("socketPath differs: %q vs %q", c1.socketPath, c2.socketPath)
	}
}

// TestDefaultCLIAddr_DefaultAndOverride pins the literal + env override
// contract cmd/start.go's own default listener bind address and
// profiles.ResolveWithoutToken's TCP terminal fallback both key off.
func TestDefaultCLIAddr_DefaultAndOverride(t *testing.T) {
	if got := DefaultCLIAddr(); got != "127.0.0.1:8442" {
		t.Errorf("DefaultCLIAddr() = %q, want %q", got, "127.0.0.1:8442")
	}
	t.Setenv("BOID_CLI_ADDR", "10.0.0.5:9999")
	if got := DefaultCLIAddr(); got != "10.0.0.5:9999" {
		t.Errorf("DefaultCLIAddr() with BOID_CLI_ADDR set = %q, want %q", got, "10.0.0.5:9999")
	}
}

// TestDefaultCACertPath_UnderXDGConfigHome pins the path resolution
// convention (mirrors profiles.ConfigPath()'s own contract).
func TestDefaultCACertPath_UnderXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/example-config-home")
	got, err := DefaultCACertPath()
	if err != nil {
		t.Fatalf("DefaultCACertPath: %v", err)
	}
	want := "/tmp/example-config-home/boid/daemon-ca.pem"
	if got != want {
		t.Errorf("DefaultCACertPath() = %q, want %q", got, want)
	}
}
