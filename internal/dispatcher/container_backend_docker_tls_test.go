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

// TestContainerBackend_Launch_DockerTLS_Disabled_NoOp pins the default (and,
// as of PR6, only production-reachable) behavior: with no
// ContainerBackendOptions.DockerTLSCA configured, LaunchOptions.DockerEnabled
// is inert — no DOCKER_* env, no extra mount. This must hold even when a job
// declares DockerEnabled, matching the userns backend's own docker-capable
// jobs (which use the per-sandbox UNIX socket dockerproxy path unaffected by
// this feature entirely).
func TestContainerBackend_Launch_DockerTLS_Disabled_NoOp(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{}) // no DockerTLSCA

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-1", DockerEnabled: true})

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	cfg := api.createCalls[0].Config
	for _, e := range cfg.Env {
		if strings.HasPrefix(e, "DOCKER_") {
			t.Errorf("Env contains %q; want no DOCKER_* vars when DockerTLSCA is unset", e)
		}
	}
	for _, m := range api.createCalls[0].HostConfig.Mounts {
		if m.Target == containerDockerTLSDir {
			t.Errorf("unexpected docker-tls mount %+v when DockerTLSCA is unset", m)
		}
	}
}

// TestContainerBackend_Launch_DockerTLS_DisabledWhenNotDockerEnabled pins
// the other half: even with a CA configured, a job that did NOT declare
// DockerEnabled gets no cert/mount/env at all.
func TestContainerBackend_Launch_DockerTLS_DisabledWhenNotDockerEnabled(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{DockerTLSCA: ca, DockerProxyAddr: "boid-dockerproxy:2376"})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-1"}) // DockerEnabled left false

	cfg := api.createCalls[0].Config
	for _, e := range cfg.Env {
		if strings.HasPrefix(e, "DOCKER_") {
			t.Errorf("Env contains %q; want no DOCKER_* vars when DockerEnabled is false", e)
		}
	}
}

// TestContainerBackend_Launch_DockerTLS_IssuesAndMountsPerJobCert is the
// positive case: DockerEnabled + a configured CA together must produce (a)
// DOCKER_HOST/DOCKER_CERT_PATH/DOCKER_TLS_VERIFY env pointing at
// DockerProxyAddr/containerDockerTLSDir, (b) a read-only bind mount at
// containerDockerTLSDir, and (c) a real cert.pem/key.pem/ca.pem trio on the
// host side of that mount that round-trips through tls.X509KeyPair (what a
// real docker client loads) and verifies against the CA.
//
// ContainerWaitFunc blocks until the test explicitly releases it: Launch
// starts containerSession.waitLoop in a background goroutine immediately
// (sess.start()), and waitLoop removes the docker-tls dir once the
// container "exits" — without this block, reading the on-disk files below
// would race against that cleanup.
func TestContainerBackend_Launch_DockerTLS_IssuesAndMountsPerJobCert(t *testing.T) {
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
	be := NewContainerBackend(api, ContainerBackendOptions{DockerTLSCA: ca, DockerProxyAddr: "boid-dockerproxy:2376"})

	issuedAt := time.Now()
	mustLaunch(t, be, sandbox.Spec{ID: "job-docker", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-docker", DockerEnabled: true})
	// Unblock waitLoop's cleanup now that the assertions below are done —
	// deferred so it runs even if a t.Fatalf below stops the test early.
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
	if env["DOCKER_HOST"] != "tcp://boid-dockerproxy:2376" {
		t.Errorf("DOCKER_HOST = %q, want tcp://boid-dockerproxy:2376", env["DOCKER_HOST"])
	}
	if env["DOCKER_CERT_PATH"] != containerDockerTLSDir {
		t.Errorf("DOCKER_CERT_PATH = %q, want %q", env["DOCKER_CERT_PATH"], containerDockerTLSDir)
	}
	if env["DOCKER_TLS_VERIFY"] != "1" {
		t.Errorf("DOCKER_TLS_VERIFY = %q, want \"1\"", env["DOCKER_TLS_VERIFY"])
	}

	var certDir string
	for _, m := range created.HostConfig.Mounts {
		if m.Target == containerDockerTLSDir {
			certDir = m.Source
			if !m.ReadOnly {
				t.Errorf("docker-tls mount is not ReadOnly: %+v", m)
			}
		}
	}
	if certDir == "" {
		t.Fatalf("no bind mount targeting %q found in %+v", containerDockerTLSDir, created.HostConfig.Mounts)
	}

	certPEM, err := os.ReadFile(filepath.Join(certDir, dockerCertFileName))
	if err != nil {
		t.Fatalf("read cert.pem: %v", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(certDir, dockerKeyFileName))
	if err != nil {
		t.Fatalf("read key.pem: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(certDir, dockerCAFileName))
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

	// Blocker 4 (PR6 codex review): the per-job cert must be short-lived
	// (perJobDockerCertValidity, 1h), not mtls.CA's default 30-day leaf
	// validity — a copy of it on a sibling must not remain usable long
	// after this job's own materialization directory is gone.
	if maxNotAfter := issuedAt.Add(perJobDockerCertValidity + 5*time.Minute); leafCert.NotAfter.After(maxNotAfter) {
		t.Errorf("leaf cert NotAfter = %v, want within ~%s of issuance (<=%v)", leafCert.NotAfter, perJobDockerCertValidity, maxNotAfter)
	}
	if leafCert.NotAfter.Sub(issuedAt) >= 24*time.Hour {
		t.Errorf("leaf cert NotAfter = %v is not meaningfully short-lived (>=24h from issuance)", leafCert.NotAfter)
	}
}

// TestContainerBackend_Launch_DockerTLS_RuntimeDir_MaterializesUnderRuntimeDir
// pins Major 11 (PR6 codex review): with ContainerBackendOptions.RuntimeDir
// set (the compose deploy's BOID_RUNTIME_DIR, bind-mounted source ==
// target into this daemon's own container — build/container/compose.yml's
// "Persistence" header comment), the per-job TLS material must be written
// under <RuntimeDir>/tls/<jobID>, not a fresh os.MkdirTemp("", ...)
// directory (this daemon container's own private /tmp — not visible to
// the sibling docker daemon DooD creates job containers through, since
// compose.yml does not bind-mount /tmp).
func TestContainerBackend_Launch_DockerTLS_RuntimeDir_MaterializesUnderRuntimeDir(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	runtimeDir := t.TempDir()

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
		DockerTLSCA:     ca,
		DockerProxyAddr: "boid-dockerproxy:2376",
		RuntimeDir:      runtimeDir,
	})

	mustLaunch(t, be, sandbox.Spec{ID: "job-docker", Argv: []string{"true"}},
		backend.LaunchOptions{JobID: "job-docker", DockerEnabled: true})
	defer close(waitBlock)

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	created := api.createCalls[0]

	var certDir string
	for _, m := range created.HostConfig.Mounts {
		if m.Target == containerDockerTLSDir {
			certDir = m.Source
		}
	}
	if certDir == "" {
		t.Fatalf("no bind mount targeting %q found in %+v", containerDockerTLSDir, created.HostConfig.Mounts)
	}

	wantDir := filepath.Join(runtimeDir, "tls", "job-docker")
	if certDir != wantDir {
		t.Errorf("cert mount Source = %q, want %q (under RuntimeDir, not a fresh MkdirTemp dir)", certDir, wantDir)
	}
	if !strings.HasPrefix(certDir, runtimeDir) {
		t.Errorf("cert mount Source = %q, want it to live under RuntimeDir %q", certDir, runtimeDir)
	}

	if _, err := os.Stat(filepath.Join(certDir, dockerCertFileName)); err != nil {
		t.Errorf("cert.pem not found under RuntimeDir-based dir: %v", err)
	}
}
