package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"testing"
	"time"
)

// withClock replaces the package clock for the duration of the test.
func withClock(t *testing.T, at *time.Time) {
	t.Helper()
	orig := now
	now = func() time.Time { return *at }
	t.Cleanup(func() { now = orig })
}

func servedLeaf(t *testing.T, cfg *tls.Config) *x509.Certificate {
	t.Helper()
	if cfg.GetCertificate == nil {
		t.Fatal("server config has no GetCertificate; the served leaf can never be renewed")
	}
	cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf
}

func TestServerTLSConfigs_RenewLeafBeforeExpiry(t *testing.T) {
	builders := map[string]func(*CA, ...string) (*tls.Config, error){
		"ServerTLSConfig":     (*CA).ServerTLSConfig,
		"ServerOnlyTLSConfig": (*CA).ServerOnlyTLSConfig,
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			clock := time.Now()
			withClock(t, &clock)
			ca, err := LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatalf("LoadOrCreate: %v", err)
			}
			cfg, err := build(ca, "127.0.0.1")
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			first := servedLeaf(t, cfg)

			clock = clock.Add(time.Hour)
			if again := servedLeaf(t, cfg); again.SerialNumber.Cmp(first.SerialNumber) != 0 {
				t.Fatal("leaf was reissued on every handshake; want it reused while fresh")
			}

			clock = first.NotAfter.Add(time.Hour)
			renewed := servedLeaf(t, cfg)
			if !clock.Before(renewed.NotAfter) || clock.Before(renewed.NotBefore) {
				t.Fatalf("served leaf not valid at %v (NotBefore=%v NotAfter=%v)", clock, renewed.NotBefore, renewed.NotAfter)
			}
		})
	}
}

// TestServerTLSConfig_HandshakeAfterInitialLeafExpiry dials the listener
// after the first leaf's NotAfter has passed.
func TestServerTLSConfig_HandshakeAfterInitialLeafExpiry(t *testing.T) {
	clock := time.Now()
	withClock(t, &clock)
	ca, err := LoadOrCreate(t.TempDir())
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

	clock = clock.Add(leafValidity + 24*time.Hour)
	clientCert, err := ca.IssueClientCert("test-client")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	clientCfg := ca.ClientTLSConfig("127.0.0.1", clientCert)
	clientCfg.Time = func() time.Time { return clock }
	serverCfg.Time = func() time.Time { return clock }

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("tls.Dial after initial leaf expiry: %v", err)
	}
	conn.Close()
}
