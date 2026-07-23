package dispatcher

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins the per-job broker client cert delivery half of
// docs/plans/phase6-cutover-followups.md §⓪ ("broker TCP wire completion"):
// analogous to container_backend_docker_tls_test.go's dockerproxy coverage,
// but for the broker's own TCP(mTLS) listener. Unlike dockerproxy (gated on
// LaunchOptions.DockerEnabled), broker TLS material is materialized
// unconditionally whenever ContainerBackendOptions.BrokerTLSCA is
// configured — every non-foreground job needs broker RPC at minimum (the
// `boid job done` completion report), so there is no per-job opt-out flag
// to test against.

// TestContainerBackend_Launch_BrokerTLS_Disabled_NoOp pins the default (and,
// as of this feature landing, still the ONLY production-reachable state
// until BrokerTLSCA is actually wired by internal/server) behavior: with no
// ContainerBackendOptions.BrokerTLSCA configured, Launch adds no
// BOID_BROKER_TLS_* env and no extra mount, regardless of what the job
// declares.
func TestContainerBackend_Launch_BrokerTLS_Disabled_NoOp(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{}) // no BrokerTLSCA

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-1"})

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	cfg := api.createCalls[0].Config
	for _, e := range cfg.Env {
		if strings.HasPrefix(e, "BOID_BROKER_TLS_") {
			t.Errorf("Env contains %q; want no BOID_BROKER_TLS_* vars when BrokerTLSCA is unset", e)
		}
	}
	for _, m := range api.createCalls[0].HostConfig.Mounts {
		if m.Target == containerBrokerTLSDir {
			t.Errorf("unexpected broker-tls mount %+v when BrokerTLSCA is unset", m)
		}
	}
}

// TestContainerBackend_Launch_BrokerTLS_IssuesAndMountsPerJobCert is the
// positive case: a configured BrokerTLSCA (+ BrokerTLSAddr) must produce
// (a) the five BOID_BROKER_TLS_* env vars pointing at containerBrokerTLSDir
// / the compose broker address / composeBrokerServiceName, (b) a read-only
// bind mount at containerBrokerTLSDir, and (c) a real cert.pem/key.pem/
// ca.pem trio on the host side of that mount that round-trips through
// tls.X509KeyPair and verifies against the CA — unconditionally, unlike
// dockerproxy's DockerEnabled gate (see this file's own header comment).
//
// ContainerWaitFunc blocks until the test explicitly releases it, same
// rationale as the docker-tls test's identical pattern: waitLoop removes
// the broker-tls dir the moment the container "exits".
func TestContainerBackend_Launch_BrokerTLS_IssuesAndMountsPerJobCert(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	addr := "boid-broker:54321"

	waitBlock := make(chan struct{})
	api := &fakeDockerAPI{
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			<-waitBlock
			resCh := make(chan container.WaitResponse, 1)
			resCh <- container.WaitResponse{StatusCode: 0}
			return client.ContainerWaitResult{Result: resCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{BrokerTLSCA: ca, BrokerTLSAddr: &addr})

	issuedAt := time.Now()
	// Deliberately NOT declaring DockerEnabled/BuiltinPolicies/anything
	// else — the point of this test is that broker TLS delivery does not
	// depend on any per-job opt-in.
	mustLaunch(t, be, sandbox.Spec{ID: "job-broker", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-broker"})
	defer close(waitBlock)

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	created := api.createCalls[0]

	env := map[string]string{}
	for _, e := range created.Config.Env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			env[k] = v
		}
	}
	if env["BOID_BROKER_TLS_ADDR"] != "boid-broker:54321" {
		t.Errorf("BOID_BROKER_TLS_ADDR = %q, want %q", env["BOID_BROKER_TLS_ADDR"], "boid-broker:54321")
	}
	if env["BOID_BROKER_TLS_CERT_PATH"] != containerBrokerTLSDir+"/cert.pem" {
		t.Errorf("BOID_BROKER_TLS_CERT_PATH = %q, want %q", env["BOID_BROKER_TLS_CERT_PATH"], containerBrokerTLSDir+"/cert.pem")
	}
	if env["BOID_BROKER_TLS_KEY_PATH"] != containerBrokerTLSDir+"/key.pem" {
		t.Errorf("BOID_BROKER_TLS_KEY_PATH = %q, want %q", env["BOID_BROKER_TLS_KEY_PATH"], containerBrokerTLSDir+"/key.pem")
	}
	if env["BOID_BROKER_TLS_CA_PATH"] != containerBrokerTLSDir+"/ca.pem" {
		t.Errorf("BOID_BROKER_TLS_CA_PATH = %q, want %q", env["BOID_BROKER_TLS_CA_PATH"], containerBrokerTLSDir+"/ca.pem")
	}
	if env["BOID_BROKER_TLS_SERVER_NAME"] != "boid-broker" {
		t.Errorf("BOID_BROKER_TLS_SERVER_NAME = %q, want %q", env["BOID_BROKER_TLS_SERVER_NAME"], "boid-broker")
	}
	// The env delivery must NOT set BOID_BROKER_SOCKET — that key belongs
	// to sandbox_builder.go's own (backend-independent) BrokerSocket
	// mount, and SendJSONFromEnv already prefers the TLS transport over it
	// whenever both are present (see withBrokerTLSEnv's own doc comment).
	if _, ok := env["BOID_BROKER_SOCKET"]; ok {
		t.Errorf("Env contains BOID_BROKER_SOCKET = %q; withBrokerTLSEnv must not touch that key", env["BOID_BROKER_SOCKET"])
	}

	var certDir string
	for _, m := range created.HostConfig.Mounts {
		if m.Target == containerBrokerTLSDir {
			certDir = m.Source
			if !m.ReadOnly {
				t.Errorf("broker-tls mount is not ReadOnly: %+v", m)
			}
		}
	}
	if certDir == "" {
		t.Fatalf("no bind mount targeting %q found in %+v", containerBrokerTLSDir, created.HostConfig.Mounts)
	}

	certPEM, err := os.ReadFile(filepath.Join(certDir, brokerCertFileName))
	if err != nil {
		t.Fatalf("read cert.pem: %v", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(certDir, brokerKeyFileName))
	if err != nil {
		t.Fatalf("read key.pem: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(certDir, brokerCAFileName))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}

	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("tls.X509KeyPair(cert.pem, key.pem): %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to parse ca.pem into a cert pool")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to PEM-decode cert.pem")
	}
	leafCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf cert DER: %v", err)
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("leaf cert does not verify against ca.pem: %v", err)
	}

	// perJobBrokerCertValidity (24h, not dockerproxy's 1h — see its own doc
	// comment for why): the leaf must be short-lived relative to mtls.CA's
	// default 30-day validity, but long enough to comfortably outlive an
	// exceptionally long-running job.
	if maxNotAfter := issuedAt.Add(perJobBrokerCertValidity + 5*time.Minute); leafCert.NotAfter.After(maxNotAfter) {
		t.Errorf("leaf cert NotAfter = %v, want within ~%s of issuance (<=%v)", leafCert.NotAfter, perJobBrokerCertValidity, maxNotAfter)
	}
	if leafCert.NotAfter.Sub(issuedAt) >= 30*24*time.Hour {
		t.Errorf("leaf cert NotAfter = %v is not meaningfully short-lived (>= mtls.CA's own 30-day default leaf validity)", leafCert.NotAfter)
	}
}

// TestContainerBackend_Launch_BrokerTLS_NilAddrPointer_EmptyEnv pins the
// defensive nil-pointer branch: a BrokerTLSCA configured with a nil
// BrokerTLSAddr pointer (a construction-ordering mistake that should not
// happen in production wiring — see ContainerBackendOptions.BrokerTLSAddr's
// own doc comment) must not panic; BOID_BROKER_TLS_ADDR is set to the empty
// string rather than omitted, so a job that somehow hits this fails loudly
// (SendJSONFromEnv's own dial attempt) instead of silently falling back to
// a stale transport.
func TestContainerBackend_Launch_BrokerTLS_NilAddrPointer_EmptyEnv(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}

	waitBlock := make(chan struct{})
	api := &fakeDockerAPI{
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			<-waitBlock
			resCh := make(chan container.WaitResponse, 1)
			resCh <- container.WaitResponse{StatusCode: 0}
			return client.ContainerWaitResult{Result: resCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{BrokerTLSCA: ca}) // BrokerTLSAddr left nil

	mustLaunch(t, be, sandbox.Spec{ID: "job-broker-nil-addr", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-broker-nil-addr"})
	defer close(waitBlock)

	created := api.createCalls[0]
	env := map[string]string{}
	for _, e := range created.Config.Env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			env[k] = v
		}
	}
	if got, ok := env["BOID_BROKER_TLS_ADDR"]; !ok || got != "" {
		t.Errorf("BOID_BROKER_TLS_ADDR = (present=%v) %q, want present and empty", ok, got)
	}
}

// TestContainerBackend_Launch_BrokerTLS_RuntimeDir_MaterializesUnderRuntimeDir
// mirrors TestContainerBackend_Launch_DockerTLS_RuntimeDir_MaterializesUnderRuntimeDir:
// with ContainerBackendOptions.RuntimeDir set, the per-job TLS material must
// be written under <RuntimeDir>/broker-tls/<jobID>, not a fresh
// os.MkdirTemp("", ...) directory invisible to the sibling docker daemon
// DooD creates job containers through.
func TestContainerBackend_Launch_BrokerTLS_RuntimeDir_MaterializesUnderRuntimeDir(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	runtimeDir := t.TempDir()
	addr := "boid-broker:54321"

	waitBlock := make(chan struct{})
	api := &fakeDockerAPI{
		ContainerWaitFunc: func(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult {
			<-waitBlock
			resCh := make(chan container.WaitResponse, 1)
			resCh <- container.WaitResponse{StatusCode: 0}
			return client.ContainerWaitResult{Result: resCh, Error: make(chan error, 1)}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{
		BrokerTLSCA:   ca,
		BrokerTLSAddr: &addr,
		RuntimeDir:    runtimeDir,
	})

	mustLaunch(t, be, sandbox.Spec{ID: "job-broker-rtdir", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-broker-rtdir"})
	defer close(waitBlock)

	created := api.createCalls[0]
	var certDir string
	for _, m := range created.HostConfig.Mounts {
		if m.Target == containerBrokerTLSDir {
			certDir = m.Source
		}
	}
	if certDir == "" {
		t.Fatalf("no bind mount targeting %q found in %+v", containerBrokerTLSDir, created.HostConfig.Mounts)
	}

	wantDir := filepath.Join(runtimeDir, "broker-tls", "job-broker-rtdir")
	if certDir != wantDir {
		t.Errorf("cert mount Source = %q, want %q (under RuntimeDir, not a fresh MkdirTemp dir)", certDir, wantDir)
	}
	if _, err := os.Stat(filepath.Join(certDir, brokerCertFileName)); err != nil {
		t.Errorf("cert.pem not found under RuntimeDir-based dir: %v", err)
	}
}

// TestContainerBackend_Launch_BrokerTLS_CleanupOnExit pins the cleanup half
// of containerSession.brokerTLSDir's retention contract: once the
// container "exits" (waitLoop runs), the per-job broker-tls directory must
// be removed, mirroring dockerTLSDir's identical contract.
func TestContainerBackend_Launch_BrokerTLS_CleanupOnExit(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	addr := "boid-broker:54321"
	runtimeDir := t.TempDir()

	api := &fakeDockerAPI{} // default ContainerWaitFunc resolves immediately
	be := NewContainerBackend(api, ContainerBackendOptions{
		BrokerTLSCA:   ca,
		BrokerTLSAddr: &addr,
		RuntimeDir:    runtimeDir,
	})

	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-broker-cleanup", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-broker-cleanup"})

	wantDir := filepath.Join(runtimeDir, "broker-tls", "job-broker-cleanup")
	// sess.Wait unblocks once waitLoop closes s.done — but the
	// specDir/dockerTLSDir/brokerTLSDir cleanup runs AFTER that close (see
	// waitLoop's own body: close(s.done) precedes the diagnostics
	// collector call, ContainerRemove, and only then the three cleanup
	// blocks), so Wait returning is not itself sufficient synchronization
	// here — unlike the transcript-spool close, which happens BEFORE
	// close(s.done) (see TestContainerSession_TranscriptSpool_
	// SurvivesContainerRemove's own doc comment for that contrast). Poll
	// briefly instead, the same race-free-without-a-fixed-sleep shape
	// api.removeCallCount() already establishes for the identical ordering
	// problem around ContainerRemove.
	if _, err := sess.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, statErr := os.Stat(wantDir)
		if os.IsNotExist(statErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker-tls dir %q still exists after container exit, want removed (err=%v)", wantDir, statErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
