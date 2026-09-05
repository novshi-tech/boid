// Package mtls provides a small per-daemon self-signed certificate
// authority used to secure the broker / git-gateway / dockerproxy TCP
// listeners (docs/plans/phase6-container-backend.md §決定5). It is
// intentionally minimal — issue short-lived leaf certificates off a CA
// persisted on disk, nothing more (no ACME, no external PKI), using only
// crypto/tls + crypto/x509 per project convention.
//
// CA generation/persistence and per-listener SERVER certs are wired into
// internal/server.Server. Per-JOB CLIENT certs are not materialized or
// distributed to any real job by this package's production callers —
// IssueClientCert exists so the mTLS handshake can be exercised end-to-end
// in tests.
package mtls

import (
	"bytes"
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

	"github.com/novshi-tech/boid/internal/atomicfile"
)

const (
	// CAFileName and KeyFileName are the on-disk names LoadOrCreate reads
	// and writes under its dir argument. The production caller
	// (internal/server.Server) points dir at ~/.local/share/boid/tls.
	CAFileName  = "ca.crt"
	KeyFileName = "ca.key"

	// caValidity is intentionally long: this is a per-daemon internal CA,
	// not rotated, so it must outlive normal daemon uptime by a wide margin.
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
// (0700) if needed.
//
// Publish uses atomicfile.PublishIfAbsent for each file independently
// instead of a plain os.WriteFile, so two daemon instances racing to boot
// against the same fresh, empty volume never observe a half-written
// ca.crt/ca.key that fails to parse.
//
// Known residual risk: PublishIfAbsent's one-winner guarantee is per FILE,
// not per (cert,key) PAIR — a race where one caller wins the ca.crt publish
// while a different caller wins the ca.key publish would produce a
// cryptographically mismatched pair. This is not guarded against here (see
// parseCA); the hazard window is "two daemon instances racing at boot
// against the same volume", not a supported topology for this daemon.
func LoadOrCreate(dir string) (*CA, error) {
	certPath := filepath.Join(dir, CAFileName)
	keyPath := filepath.Join(dir, KeyFileName)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		// The key is written 0600 at create time (below); reusing an
		// existing key that has since gained broader permissions (e.g.
		// restored from an archive as 0644) would silently trust
		// whatever protection the file happens to have today instead of
		// the guarantee this package promises. Reject rather than
		// silently chmod — a caller that wants the key readable more
		// broadly should say so explicitly, not have LoadOrCreate paper
		// over it.
		info, err := os.Stat(keyPath)
		if err != nil {
			return nil, fmt.Errorf("mtls: stat ca key: %w", err)
		}
		if info.Mode().Perm()&0o177 != 0 {
			return nil, fmt.Errorf("mtls: ca key %s has unsafe permissions %#o (must be 0600 — same as create-time)", keyPath, info.Mode().Perm())
		}
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
	publishedCertPEM, err := atomicfile.PublishIfAbsent(certPath, 0o600, newCertPEM)
	if err != nil {
		return nil, fmt.Errorf("mtls: write ca cert: %w", err)
	}
	publishedKeyPEM, err := atomicfile.PublishIfAbsent(keyPath, 0o600, newKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("mtls: write ca key: %w", err)
	}
	if !bytes.Equal(publishedCertPEM, newCertPEM) || !bytes.Equal(publishedKeyPEM, newKeyPEM) {
		// A concurrent LoadOrCreate call already published one or both
		// files first; re-parse whatever combination is actually on disk
		// rather than trusting our own freshly generated `ca` in memory.
		return parseCA(publishedCertPEM, publishedKeyPEM)
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

// parseCA parses cert/key PEM independently loaded from disk and verifies
// they actually form a matching pair — guarding against a cert from one boot
// ending up next to a key from a later boot (e.g. one file's publish fails
// with ENOSPC while the other's succeeds), each individually well-formed PEM
// but cryptographically unrelated, which x509.ParseCertificate/
// ParseECPrivateKey alone would not catch.
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
	if err := verifyCertKeyPairMatch(cert, key); err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key}, nil
}

// verifyCertKeyPairMatch reports an error unless cert's public key and
// key's public component are the same EC point — i.e. key is actually the
// private half of cert, not just an independently valid key. See parseCA's
// doc comment for the failure scenario this closes.
func verifyCertKeyPairMatch(cert *x509.Certificate, key *ecdsa.PrivateKey) error {
	certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("mtls: ca cert public key is %T, want *ecdsa.PublicKey (every key this package generates is ECDSA)", cert.PublicKey)
	}
	keyPub := key.Public().(*ecdsa.PublicKey) //nolint:forcetypeassert // key.Public() on an *ecdsa.PrivateKey always returns *ecdsa.PublicKey
	if certPub.X.Cmp(keyPub.X) != 0 || certPub.Y.Cmp(keyPub.Y) != 0 {
		return fmt.Errorf("mtls: ca.crt and ca.key do not form a matching pair (public keys differ) — this typically means one file's atomic publish succeeded on a prior boot while the other's failed (e.g. ENOSPC) and a later boot published a fresh replacement for only one of the two; remove both files under this CA's directory (rm ca.crt ca.key under tls/) so LoadOrCreate regenerates a fresh, consistent pair — safe per docs/plans/volume-only-daemon.md §論点 d, which treats this material as volatile/regenerable")
	}
	return nil
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

// CertPEM returns this CA's own certificate, PEM-encoded — the "ca.pem"
// file docker's DOCKER_CERT_PATH convention expects alongside a leaf
// cert/key pair (see EncodeCertPEM for the leaf half).
func (ca *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// issueLeaf signs a fresh leaf certificate for subject cn, valid for
// validity from now. hosts populates DNS/IP SANs (server certs only; empty
// for client certs).
func (ca *CA) issueLeaf(cn string, hosts []string, eku []x509.ExtKeyUsage, validity time.Duration) (tls.Certificate, error) {
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
		NotAfter:     time.Now().Add(validity),
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
	return ca.issueLeaf(cn, hosts, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, leafValidity)
}

// IssueClientCert issues a leaf client-authentication certificate
// identified by cn, valid for the default leafValidity (30 days). No
// production caller uses this — it exists so tests can exercise a real
// mTLS handshake against a ServerTLSConfig listener. A real per-job client
// cert must use IssueShortLivedClientCert instead (see its own doc comment
// for why 30 days is too long for that use case).
func (ca *CA) IssueClientCert(cn string) (tls.Certificate, error) {
	return ca.issueLeaf(cn, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, leafValidity)
}

// IssueShortLivedClientCert issues a client-authentication leaf
// certificate identified by cn, like IssueClientCert, but with a
// caller-supplied validity window instead of the default 30-day
// leafValidity.
//
// This is what a per-job dockerproxy client cert must use, not
// IssueClientCert: a per-job cert's trust boundary is meant to track the
// job it was issued for, but a copy of the cert made onto a sibling before
// the job exits would otherwise stay valid — and usable against the
// dockerproxy listener — long after the job and the daemon's own record of
// it are gone. A short (typically 1h) validity bounds that exposure window.
func (ca *CA) IssueShortLivedClientCert(cn string, validity time.Duration) (tls.Certificate, error) {
	return ca.issueLeaf(cn, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, validity)
}

// ServerTLSConfig builds a tls.Config for a TCP listener: presents a fresh
// server cert for hosts and requires (and verifies) a client certificate
// signed by this CA — mutual TLS. Any connection without a CA-signed client
// cert is rejected at the handshake; the server does not inspect *which*
// identity the client cert names.
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

// ServerOnlyTLSConfig builds a tls.Config for a TCP listener that
// authenticates only the SERVER side (a standard, non-mutual TLS config) —
// unlike ServerTLSConfig, it does not require or verify a client
// certificate.
//
// This is for a listener (the git gateway) whose per-connection
// authorization is already fully handled at the application layer by an
// existing per-job bearer token, with no per-job client cert ever wired for
// it — requiring one anyway (ServerTLSConfig's unconditional
// tls.RequireAndVerifyClientCert) would make the listener unusable by any
// real client.
func (ca *CA) ServerOnlyTLSConfig(hosts ...string) (*tls.Config, error) {
	cert, err := ca.IssueServerCert(hosts...)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
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

// EncodeCertPEM PEM-encodes a leaf certificate issued by IssueServerCert /
// IssueClientCert into the (certPEM, keyPEM) pair most file-based TLS
// consumers expect — docker's DOCKER_CERT_PATH convention (cert.pem +
// key.pem + ca.pem, see CA.CertPEM for the third file) among them.
// tls.Certificate itself is Go-internal (raw DER bytes plus a
// crypto.PrivateKey interface value) rather than a file format, so this is
// the one conversion step every such consumer needs.
func EncodeCertPEM(cert tls.Certificate) (certPEM, keyPEM []byte, err error) {
	if len(cert.Certificate) == 0 {
		return nil, nil, fmt.Errorf("mtls: certificate has no DER bytes")
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})

	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("mtls: unsupported private key type %T (every key this package issues is *ecdsa.PrivateKey)", cert.PrivateKey)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: marshal private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
