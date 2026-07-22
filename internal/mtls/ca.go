// Package mtls provides a small per-daemon self-signed certificate
// authority used to secure the broker / git-gateway / dockerproxy TCP
// listeners introduced by docs/plans/phase6-container-backend.md §PR4
// (§決定5: "gateway / broker / dockerproxy はサービス名 (DNS) + TCP (mTLS) で
// 到達する"). It is intentionally minimal — issue short-lived leaf
// certificates off a CA persisted on disk, nothing more (no ACME, no
// external PKI). crypto/tls + crypto/x509 only, per project convention
// (CLAUDE.md: 外部ライブラリは最小限。標準ライブラリで実現できるものは追加しない).
//
// Scope note (PR4): CA generation/persistence and per-listener SERVER
// certs are real and wired into internal/server.Server. Per-JOB CLIENT
// certs (§決定5's "per-job 短命 client cert") are NOT materialized or
// distributed to any real job by this package's production callers —
// IssueClientCert exists so the mTLS handshake can be exercised
// end-to-end in tests today. Wiring an actual per-job identity (env
// delivery, container-local materialization, DOCKER_CERT_PATH, ...) is
// PR6 scope per the plan doc.
package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// CAFileName and KeyFileName are the on-disk names LoadOrCreate reads
	// and writes under its dir argument. The production caller
	// (internal/server.Server) points dir at
	// ~/.local/share/boid/tls — the same data-dir convention
	// internal/dispatcher.LoadOrCreateKey's web_secret file uses.
	CAFileName  = "ca.crt"
	KeyFileName = "ca.key"

	// caValidity is intentionally long: this is a per-daemon internal CA,
	// not rotated by PR4, so it must outlive normal daemon uptime by a
	// wide margin. Rotation policy is out of scope for PR4.
	caValidity = 10 * 365 * 24 * time.Hour

	// leafValidity bounds every per-listener server cert and test-only
	// client cert issued from the CA. Leaves are never persisted — a
	// fresh one is issued each time IssueServerCert/IssueClientCert runs
	// (typically once per daemon start) — so a short validity window
	// costs nothing operationally.
	leafValidity = 30 * 24 * time.Hour
)

// CA is a loaded or freshly generated self-signed internal certificate
// authority.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// LoadOrCreate loads ca.crt/ca.key from dir, or generates and persists a
// new self-signed CA there if either file is missing. dir is created
// (0700) if needed. Mirrors internal/dispatcher.LoadOrCreateKey's
// load-or-generate-and-persist shape for the web_secret file.
func LoadOrCreate(dir string) (*CA, error) {
	certPath := filepath.Join(dir, CAFileName)
	keyPath := filepath.Join(dir, KeyFileName)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		return parseCA(certPEM, keyPEM)
	}
	if certErr != nil && !os.IsNotExist(certErr) {
		return nil, fmt.Errorf("mtls: read ca cert: %w", certErr)
	}
	if keyErr != nil && !os.IsNotExist(keyErr) {
		return nil, fmt.Errorf("mtls: read ca key: %w", keyErr)
	}

	ca, newCertPEM, newKeyPEM, err := generateCA()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mtls: mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(certPath, newCertPEM, 0o600); err != nil {
		return nil, fmt.Errorf("mtls: write ca cert: %w", err)
	}
	if err := os.WriteFile(keyPath, newKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("mtls: write ca key: %w", err)
	}
	return ca, nil
}

func generateCA() (*CA, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mtls: generate ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "boid internal CA", Organization: []string{"boid"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mtls: create ca cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mtls: parse ca cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mtls: marshal ca key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &CA{cert: cert, key: key}, certPEM, keyPEM, nil
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("mtls: no PEM block found in ca cert")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mtls: parse ca cert: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("mtls: no PEM block found in ca key")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mtls: parse ca key: %w", err)
	}
	return &CA{cert: cert, key: key}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("mtls: generate serial: %w", err)
	}
	return serial, nil
}

// CertPool returns an x509.CertPool containing just this CA's certificate —
// suitable for tls.Config.ClientCAs (verify client certs against this CA)
// or RootCAs (verify a server cert issued by this CA).
func (ca *CA) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// issueLeaf signs a fresh leaf certificate for subject cn. hosts populates
// DNS/IP SANs (server certs only; empty for client certs).
func (ca *CA) issueLeaf(cn string, hosts []string, eku []x509.ExtKeyUsage) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"boid"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  eku,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: sign leaf cert: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}

// IssueServerCert issues a leaf certificate for a TCP listener, valid for
// the given DNS names / IP addresses (hosts). The first host becomes the
// certificate's CommonName; hosts may be empty (a nameless cert still
// works for tests that skip hostname verification via ServerName). Never
// persisted — issue fresh on every listener bind.
func (ca *CA) IssueServerCert(hosts ...string) (tls.Certificate, error) {
	cn := "boid-server"
	if len(hosts) > 0 {
		cn = hosts[0]
	}
	return ca.issueLeaf(cn, hosts, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
}

// IssueClientCert issues a leaf client-authentication certificate
// identified by cn. Production callers do not use this in PR4 — it exists
// so tests (and PR6's per-job client cert issuance) can exercise a real
// mTLS handshake against a ServerTLSConfig listener.
func (ca *CA) IssueClientCert(cn string) (tls.Certificate, error) {
	return ca.issueLeaf(cn, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

// ServerTLSConfig builds a tls.Config for a TCP listener: presents a fresh
// server cert for hosts and requires (and verifies) a client certificate
// signed by this CA — mutual TLS. This is the "skeleton" mTLS server auth
// PR4 delivers: any connection without a CA-signed client cert is rejected
// at the handshake, but the server does not yet inspect *which* identity
// the client cert names (per-job scoping is PR6 — §決定5's "per-job
// client cert" note).
func (ca *CA) ServerTLSConfig(hosts ...string) (*tls.Config, error) {
	cert, err := ca.IssueServerCert(hosts...)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    ca.CertPool(),
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig builds a tls.Config for connecting to a listener secured
// by ServerTLSConfig: trusts this CA as the server's root and presents
// cert (from IssueClientCert) as the client's identity. serverName must
// match a DNS/IP SAN on the listener's server cert (or be set to skip
// verification in tests, which callers should avoid in production code).
func (ca *CA) ClientTLSConfig(serverName string, cert tls.Certificate) *tls.Config {
	return &tls.Config{
		RootCAs:      ca.CertPool(),
		Certificates: []tls.Certificate{cert},
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}
}
