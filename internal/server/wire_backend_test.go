package server

import (
	"os"
	"testing"

	"github.com/moby/moby/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/version"
)

// This file pins sandboxBackendForConfig — the backend construction wiring
// buildRuntime uses to build the runner's SandboxBackend — end to end,
// producing the backend.SandboxBackend Runner.Backend is set to.
//
// PR-4 (docs/plans/volume-only-daemon.md §論点e) removed the userns backend
// and the sandbox.backend config option entirely: container is now the
// only sandbox backend, so sandboxBackendForConfig always constructs a
// containerBackend and never returns (nil, nil) — the userns/nil-config
// pinning tests this file used to carry (TestSandboxBackendForConfig_
// Userns_ReturnsNil, _NilConfig_ReturnsNil, _Userns_BrokerTLSIgnored) were
// removed alongside that behavior, not just renamed.
//
// client.New(client.FromEnv) does not dial docker eagerly (see
// sandboxBackendForConfig's own doc comment), so none of these tests need a
// live docker daemon.

// TestSandboxBackendForConfig_ReturnsContainerBackend pins the sole
// production path: sandboxBackendForConfig always produces a real
// containerBackend now.
func TestSandboxBackendForConfig_ReturnsContainerBackend(t *testing.T) {
	be := sandboxBackendForConfig(dockerClientForTest(t), "install-1", t.TempDir(), t.TempDir(), nil, nil)
	if be == nil {
		t.Fatal("backend = nil, want a non-nil containerBackend")
	}
	if !dispatcher.IsContainerBackend(be) {
		t.Errorf("backend = %T, want a *dispatcher.containerBackend", be)
	}
}

// TestSandboxBackendForConfig_WiresDiagnosticsCollector pins [Major 7, PR7
// codex review]: sandboxBackendForConfig must wire a real
// DiagnosticsCollector (dispatcher.NewDefaultDiagnosticsCollector) into the
// containerBackend it constructs — before that fix, production wiring left
// it nil (NewContainerBackend's own doc comment: "PR5 leaves this nil (no
// consumer yet)"), so an OOM-killed or setup-failure job container was
// removed with no diagnostic capture at all.
func TestSandboxBackendForConfig_WiresDiagnosticsCollector(t *testing.T) {
	be := sandboxBackendForConfig(dockerClientForTest(t), "install-1", t.TempDir(), t.TempDir(), nil, nil)
	if !dispatcher.ContainerBackendHasDiagnosticsCollector(be) {
		t.Error("containerBackend was constructed without a DiagnosticsCollector, want NewDefaultDiagnosticsCollector wired")
	}
}

// TestSandboxBackendForConfig_WiresDaemonUIDGID pins the PR9 fix
// (docs/plans/phase6-cutover-followups.md's e2e-container job debugging
// trail): sandboxBackendForConfig must pass the DAEMON's own actual
// os.Getuid()/os.Getgid() through to ContainerBackendOptions.UID/GID —
// before this fix neither was ever set, so every job container silently
// ran as ContainerBackendOptions' own 1000:1000 default regardless of
// what uid the daemon itself (and so its bind-mounted, daemon-uid-owned
// workspace home directories) actually ran as. This test's own process
// uid is a proxy for "the daemon's own uid" (os.Getuid() is deterministic
// per-process, exactly what sandboxBackendForConfig itself calls).
func TestSandboxBackendForConfig_WiresDaemonUIDGID(t *testing.T) {
	be := sandboxBackendForConfig(dockerClientForTest(t), "install-1", t.TempDir(), t.TempDir(), nil, nil)
	gotUID, gotGID, ok := dispatcher.ContainerBackendUIDGID(be)
	if !ok {
		t.Fatal("ContainerBackendUIDGID: be is not a containerBackend")
	}
	wantUID, wantGID := os.Getuid(), os.Getgid()
	if wantUID == 0 || wantGID == 0 {
		t.Skip("test process is running as root; ContainerBackendOptions.UID/GID's own root-rejection would mask this test's assertion")
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("containerBackend uid:gid = %d:%d, want the daemon's own os.Getuid()/os.Getgid() = %d:%d", gotUID, gotGID, wantUID, wantGID)
	}
}

// TestSandboxBackendForConfig_WiresBOIDImageEnv pins docs/plans/
// release-onboarding.md 穴4/PR4's codex review Blocker: a daemon running
// under compose can successfully pull its OWN image (build/container/
// compose.yml's `image:` line resolves BOID_IMAGE, e.g. a GHCR ref) while
// every job it dispatches still falls back to re-deriving a DIFFERENT
// default from version.DefaultContainerImage() evaluated inside the daemon
// process — which, for any non-exact-release build (the common case: a
// GHCR ":latest" pull, not a tagged release), is the bare, registry-less
// "boid-runner:latest" the whole of 穴4 exists to stop defaulting to.
// sandboxBackendForConfig must thread BOID_IMAGE straight through to
// ContainerBackendOptions.DefaultImage so the daemon and its own job
// containers always agree on the same ref.
func TestSandboxBackendForConfig_WiresBOIDImageEnv(t *testing.T) {
	t.Setenv("BOID_IMAGE", "ghcr.io/novshi-tech/boid-runner:v9.9.9")

	be := sandboxBackendForConfig(dockerClientForTest(t), "install-1", t.TempDir(), t.TempDir(), nil, nil)
	got, ok := dispatcher.ContainerBackendDefaultImage(be)
	if !ok {
		t.Fatal("ContainerBackendDefaultImage: be is not a containerBackend")
	}
	if want := "ghcr.io/novshi-tech/boid-runner:v9.9.9"; got != want {
		t.Errorf("containerBackend default image = %q, want %q (BOID_IMAGE passed through)", got, want)
	}
}

// TestSandboxBackendForConfig_NoBOIDImageEnv_FallsBackToVersionDefault pins
// the other half: outside compose (bare `boid start`, or any deploy that
// hasn't set BOID_IMAGE) sandboxBackendForConfig must leave DefaultImage
// empty so NewContainerBackend's own version.DefaultContainerImage()
// fallback stays in charge, unchanged from before this fix.
func TestSandboxBackendForConfig_NoBOIDImageEnv_FallsBackToVersionDefault(t *testing.T) {
	t.Setenv("BOID_IMAGE", "")

	be := sandboxBackendForConfig(dockerClientForTest(t), "install-1", t.TempDir(), t.TempDir(), nil, nil)
	got, ok := dispatcher.ContainerBackendDefaultImage(be)
	if !ok {
		t.Fatal("ContainerBackendDefaultImage: be is not a containerBackend")
	}
	if want := version.DefaultContainerImage(); got != want {
		t.Errorf("containerBackend default image = %q, want version.DefaultContainerImage() = %q", got, want)
	}
}

// TestSandboxBackendForConfig_WiresBrokerTLS pins the broker TCP wire
// followup's own plumbing (docs/plans/phase6-cutover-followups.md §⓪):
// sandboxBackendForConfig must pass a non-nil brokerTLSCA/brokerTLSAddr
// straight through into ContainerBackendOptions.BrokerTLSCA/BrokerTLSAddr,
// the same "override wins, nothing silently dropped" contract the other
// options fields above already have. The addr pointer is dereferenced fresh
// by dispatcher.ContainerBackendBrokerTLS (the same late-binding indirection
// containerBackend.Launch itself uses) — this test writes into the
// pointed-at string AFTER calling sandboxBackendForConfig, mirroring how
// Server.Start actually populates srv.brokerTLSSandboxAddr only after
// buildRuntime (and so this call) has already run, to confirm the wiring
// really is late-bound and not resolved once at construction time.
func TestSandboxBackendForConfig_WiresBrokerTLS(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	var addr string

	be := sandboxBackendForConfig(dockerClientForTest(t), "install-1", t.TempDir(), t.TempDir(), ca, &addr)

	gotAddr, hasCA, ok := dispatcher.ContainerBackendBrokerTLS(be)
	if !ok {
		t.Fatal("ContainerBackendBrokerTLS: be is not a containerBackend")
	}
	if !hasCA {
		t.Error("containerBackend has no BrokerTLSCA wired, want the one sandboxBackendForConfig was given")
	}
	if gotAddr != "" {
		t.Errorf("BrokerTLSAddr before the late-bound pointer is written = %q, want empty", gotAddr)
	}

	// Simulate Server.Start populating srv.brokerTLSSandboxAddr once the
	// broker's TLS listener is actually bound, strictly after this
	// function (and so the containerBackend construction above) already
	// ran.
	addr = "boid-broker:54321"
	gotAddr, _, _ = dispatcher.ContainerBackendBrokerTLS(be)
	if gotAddr != "boid-broker:54321" {
		t.Errorf("BrokerTLSAddr after the late-bound pointer is written = %q, want %q (dereferenced fresh, not resolved at construction time)", gotAddr, "boid-broker:54321")
	}
}

// dockerClientForTest builds the docker client sandboxBackendForConfig now
// takes as a parameter rather than constructing for itself (see its own doc
// comment: buildRuntime builds exactly one and shares it with the
// daemon-state-volume self-inspection). client.New(client.FromEnv) does not
// dial, so this needs no live engine — the same property the file header
// above already relies on, just moved one call up.
func dockerClientForTest(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return cli
}
