package mtls_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/mtls"
)

// TestLoadOrCreate_GeneratesAndPersists pins the on-disk materialize path
// (docs/plans/phase6-container-backend.md §PR4: "per-daemon の internal CA
// を生成... boid data dir に保存") and its permissions.
func TestLoadOrCreate_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()

	ca, err := mtls.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if ca == nil {
		t.Fatal("LoadOrCreate returned nil CA")
	}

	certPath := filepath.Join(dir, mtls.CAFileName)
	keyPath := filepath.Join(dir, mtls.KeyFileName)

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat ca cert: %v", err)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat ca key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("ca.key perm = %o, want 0600", perm)
	}
	if certInfo.Size() == 0 || keyInfo.Size() == 0 {
		t.Fatal("ca.crt / ca.key written empty")
	}
}

// TestLoadOrCreate_ReusesExisting pins "既存を再利用" (§PR4): a second call
// against the same dir must load the identical CA rather than regenerating
// (a regenerated CA would invalidate any already-issued leaf certs).
func TestLoadOrCreate_ReusesExisting(t *testing.T) {
	dir := t.TempDir()

	first, err := mtls.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	firstCert, err := first.IssueServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("issue server cert: %v", err)
	}

	second, err := mtls.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}

	// A leaf issued by `first` must verify against `second`'s cert pool —
	// this only holds if both loaded the same CA key material.
	leaf, err := x509.ParseCertificate(firstCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: second.CertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("leaf issued by first CA does not verify against second CA's pool (CA was regenerated instead of reused): %v", err)
	}
}

// TestLoadOrCreate_RejectsUnsafeKeyPermissions pins the fix for codex
// review finding [Major 3] on PR4: an existing ca.key that has gained
// permissions broader than the 0600 LoadOrCreate itself writes at
// create-time (e.g. restored from an archive as 0644) must be rejected,
// not silently reused as-is.
func TestLoadOrCreate_RejectsUnsafeKeyPermissions(t *testing.T) {
	dir := t.TempDir()

	if _, err := mtls.LoadOrCreate(dir); err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}

	keyPath := filepath.Join(dir, mtls.KeyFileName)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod ca key: %v", err)
	}

	if _, err := mtls.LoadOrCreate(dir); err == nil {
		t.Fatal("LoadOrCreate succeeded against a 0644 ca.key; want an unsafe-permissions error")
	}
}

// TestServerTLSConfig_RoundTrip is the generic (backend-agnostic) handshake
// pin: a client presenting a CA-issued cert completes a real TCP+TLS
// handshake against a listener built from ServerTLSConfig, and app data
// flows both ways. The three subsystem-specific tests
// (Test{Broker,GitGatewayTCPListener,DockerProxyTCPListener}_MutualTLSHandshake)
// exercise the same CA plumbing through each subsystem's real production
// listener/server type.
func TestServerTLSConfig_RoundTrip(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	serverCfg, err := ca.ServerTLSConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	serverErrCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErrCh <- err
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err != nil {
			serverErrCh <- err
			return
		}
		if string(buf) != "hello" {
			serverErrCh <- err
			return
		}
		_, err = conn.Write([]byte("world"))
		serverErrCh <- err
	}()

	clientCert, err := ca.IssueClientCert("test-client")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	clientCfg := ca.ClientTLSConfig("127.0.0.1", clientCert)

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "world" {
		t.Fatalf("got %q, want %q", buf, "world")
	}
	if err := <-serverErrCh; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// TestServerTLSConfig_RejectsConnectionWithoutClientCert pins the "無い接続
// は拒否する" skeleton-mTLS requirement from §PR4: a TLS client that never
// presents a certificate must fail the handshake, not merely be denied at
// the application layer.
func TestServerTLSConfig_RejectsConnectionWithoutClientCert(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	serverCfg, err := ca.ServerTLSConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	// Accept() on a tls.Listener returns before the handshake completes
	// (it's driven lazily by the first Read/Write/Handshake call), so the
	// server-side rejection is observed by explicitly calling Handshake()
	// here and reporting its result back to the test goroutine.
	serverErrCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErrCh <- err
			return
		}
		defer conn.Close()
		serverErrCh <- conn.(*tls.Conn).Handshake()
	}()

	// No Certificates: an unauthenticated client dialing a
	// RequireAndVerifyClientCert listener. InsecureSkipVerify sidesteps
	// server-cert verification so the failure we observe is specifically
	// the server's client-cert requirement, not an unrelated root-trust
	// error.
	//
	// Note: under TLS 1.3, tls.Dial's own handshake can complete
	// successfully from the client's point of view even though the
	// server is about to reject the connection — the server only
	// validates the (empty) client certificate after processing the
	// client's Finished message, i.e. after the client already considers
	// the handshake done. So this test doesn't treat a successful Dial
	// as proof of anything; it drives a Write/Read to observe the
	// server's rejection surfacing on the wire, and separately asserts
	// the server side actually errored.
	clientCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	conn, dialErr := tls.Dial("tcp", ln.Addr().String(), clientCfg)

	var clientErr error
	switch {
	case dialErr != nil:
		clientErr = dialErr
	default:
		defer conn.Close()
		if _, werr := conn.Write([]byte("x")); werr != nil {
			clientErr = werr
		} else {
			buf := make([]byte, 1)
			_, clientErr = conn.Read(buf)
		}
	}

	if serverErr := <-serverErrCh; serverErr == nil {
		t.Fatal("server accepted a connection with no client certificate; want a ClientAuth rejection")
	}
	if clientErr == nil {
		t.Fatal("client I/O succeeded despite presenting no client certificate; want the server's rejection to surface")
	}
}

// TestEncodeCertPEM_RoundTrips pins docs/plans/phase6-container-backend.md
// §PR6/§決定5's per-job client cert delivery: a leaf cert/key issued by
// IssueClientCert must PEM-encode into a pair that (a) parses back with the
// stdlib's own tls.X509KeyPair (the exact function docker's client library
// uses to load DOCKER_CERT_PATH's cert.pem+key.pem) and (b) is verifiable
// against the issuing CA's own CertPEM.
func TestEncodeCertPEM_RoundTrips(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	leaf, err := ca.IssueClientCert("job-abc123")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}

	certPEM, keyPEM, err := mtls.EncodeCertPEM(leaf)
	if err != nil {
		t.Fatalf("EncodeCertPEM: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("EncodeCertPEM returned empty cert or key PEM")
	}

	// tls.X509KeyPair is what a real docker client (or any net/tls-based
	// consumer of DOCKER_CERT_PATH) uses to load cert.pem+key.pem.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("tls.X509KeyPair(certPEM, keyPEM): %v", err)
	}

	// The leaf must verify against the CA's own PEM (the "ca.pem" file
	// docker's convention also expects).
	caPEM := ca.CertPEM()
	if len(caPEM) == 0 {
		t.Fatal("CA.CertPEM() returned empty PEM")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to parse CA.CertPEM() output back into a cert pool")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to PEM-decode EncodeCertPEM's cert output")
	}
	leafCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf cert DER: %v", err)
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("leaf cert does not verify against CA.CertPEM(): %v", err)
	}
}

// TestIssueShortLivedClientCert_ValidityWindow pins Blocker 4 (PR6 codex
// review): a per-job dockerproxy client cert must carry a validity window
// bounded by the caller-supplied duration, not the 30-day leafValidity
// IssueClientCert/IssueServerCert use — a job could copy its cert to a
// sibling before exiting, and a long-lived cert would stay usable against
// the dockerproxy TCP listener long after the job (and its own
// materialization directory) are gone.
func TestIssueShortLivedClientCert_ValidityWindow(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	before := time.Now()
	leaf, err := ca.IssueShortLivedClientCert("job-abc123", time.Hour)
	if err != nil {
		t.Fatalf("IssueShortLivedClientCert: %v", err)
	}
	after := time.Now()

	block, _ := pem.Decode(func() []byte {
		certPEM, _, err := mtls.EncodeCertPEM(leaf)
		if err != nil {
			t.Fatalf("EncodeCertPEM: %v", err)
		}
		return certPEM
	}())
	if block == nil {
		t.Fatal("failed to PEM-decode the issued leaf cert")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf cert DER: %v", err)
	}

	// NotAfter must be within ~1h of issuance — the whole point of this
	// API. A generous 5-minute buffer absorbs test execution time without
	// weakening the "short-lived, not 30-day" assertion.
	maxNotAfter := after.Add(time.Hour + 5*time.Minute)
	if parsed.NotAfter.After(maxNotAfter) {
		t.Errorf("NotAfter = %v, want within ~1h of issuance (<=%v)", parsed.NotAfter, maxNotAfter)
	}
	minNotAfter := before.Add(time.Hour - 5*time.Minute)
	if parsed.NotAfter.Before(minNotAfter) {
		t.Errorf("NotAfter = %v, want at least ~1h after issuance (>=%v)", parsed.NotAfter, minNotAfter)
	}

	// Sanity: meaningfully shorter than the default 30-day leaf validity
	// IssueClientCert uses, so this test would fail if a future edit
	// accidentally routed IssueShortLivedClientCert through the same
	// leafValidity constant again.
	if parsed.NotAfter.Sub(before) >= 24*time.Hour {
		t.Errorf("NotAfter = %v is not meaningfully short-lived (>=24h from issuance)", parsed.NotAfter)
	}
}

func TestEncodeCertPEM_RejectsEmptyCertificate(t *testing.T) {
	if _, _, err := mtls.EncodeCertPEM(tls.Certificate{}); err == nil {
		t.Fatal("expected an error for a tls.Certificate with no DER bytes, got nil")
	}
}
