package server

import (
	"os"
	"testing"

	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/mtls"
)

// This file pins sandboxBackendForConfig — the config-driven backend
// selection wiring docs/plans/phase6-container-backend.md §PR7 adds to
// buildRuntime (the "config 公開 (cutover)" TODO) — end to end from a
// config.Config value to the backend.SandboxBackend Runner.Backend would be
// set to.

// TestSandboxBackendForConfig_Userns_ReturnsNil pins the default/safe path:
// an unset (or explicit "userns") sandbox.backend must return (nil, nil) so
// buildRuntime never touches runner.Backend and Runner.sandboxBackend()
// keeps constructing its own usernsBackend, exactly as every pre-PR7
// deployment already does.
func TestSandboxBackendForConfig_Userns_ReturnsNil(t *testing.T) {
	be, err := sandboxBackendForConfig(config.DefaultConfig(), "install-1", t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("sandboxBackendForConfig: %v", err)
	}
	if be != nil {
		t.Fatalf("backend = %v, want nil for the default userns config", be)
	}
}

// TestSandboxBackendForConfig_NilConfig_ReturnsNil pins the defensive nil
// guard: a nil *config.Config (e.g. a caller that skipped config.Load's
// error path) must not panic and must resolve to the safe userns default.
func TestSandboxBackendForConfig_NilConfig_ReturnsNil(t *testing.T) {
	be, err := sandboxBackendForConfig(nil, "install-1", t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("sandboxBackendForConfig: %v", err)
	}
	if be != nil {
		t.Fatalf("backend = %v, want nil for a nil config", be)
	}
}

// TestSandboxBackendForConfig_Container_ReturnsContainerBackend pins the
// opt-in cutover path: `sandbox.backend: container` must produce a real
// containerBackend (docs/plans/phase6-container-backend.md §決定11) — the
// plan's "test: config で sandbox.backend: container → Runner.sandboxBackend()
// が containerBackend を返すことを pin" requirement, exercised at the
// config-to-backend-value layer (Runner.Backend's own "override wins" wiring
// is independently pinned in internal/dispatcher's backend wiring tests).
//
// client.New(client.FromEnv) does not dial docker eagerly (see
// sandboxBackendForConfig's own doc comment), so this test needs no live
// docker daemon.
func TestSandboxBackendForConfig_Container_ReturnsContainerBackend(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Sandbox.Backend = config.SandboxBackendContainer

	be, err := sandboxBackendForConfig(cfg, "install-1", t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("sandboxBackendForConfig: %v", err)
	}
	if be == nil {
		t.Fatal("backend = nil, want a non-nil containerBackend")
	}
	if !dispatcher.IsContainerBackend(be) {
		t.Errorf("backend = %T, want a *dispatcher.containerBackend", be)
	}
}

// TestSandboxBackendForConfig_Container_WiresDiagnosticsCollector pins
// [Major 7, PR7 codex review]: sandboxBackendForConfig must wire a real
// DiagnosticsCollector (dispatcher.NewDefaultDiagnosticsCollector) into the
// containerBackend it constructs — before this fix, production wiring left
// it nil (NewContainerBackend's own doc comment: "PR5 leaves this nil (no
// consumer yet)"), so an OOM-killed or setup-failure job container was
// removed with no diagnostic capture at all.
func TestSandboxBackendForConfig_Container_WiresDiagnosticsCollector(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Sandbox.Backend = config.SandboxBackendContainer

	be, err := sandboxBackendForConfig(cfg, "install-1", t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("sandboxBackendForConfig: %v", err)
	}
	if !dispatcher.ContainerBackendHasDiagnosticsCollector(be) {
		t.Error("containerBackend was constructed without a DiagnosticsCollector, want NewDefaultDiagnosticsCollector wired")
	}
}

// TestSandboxBackendForConfig_Container_WiresDaemonUIDGID pins the PR9 fix
// (docs/plans/phase6-cutover-followups.md's e2e-container job debugging
// trail): sandboxBackendForConfig must pass the DAEMON's own actual
// os.Getuid()/os.Getgid() through to ContainerBackendOptions.UID/GID —
// before this fix neither was ever set, so every job container silently
// ran as ContainerBackendOptions' own 1000:1000 default regardless of
// what uid the daemon itself (and so its bind-mounted, daemon-uid-owned
// workspace home directories) actually ran as. This test's own process
// uid is a proxy for "the daemon's own uid" (os.Getuid() is deterministic
// per-process, exactly what sandboxBackendForConfig itself calls).
func TestSandboxBackendForConfig_Container_WiresDaemonUIDGID(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Sandbox.Backend = config.SandboxBackendContainer

	be, err := sandboxBackendForConfig(cfg, "install-1", t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("sandboxBackendForConfig: %v", err)
	}
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

// TestSandboxBackendForConfig_Container_WiresBrokerTLS pins the broker TCP
// wire followup's own plumbing (docs/plans/phase6-cutover-followups.md
// §⓪): sandboxBackendForConfig must pass a non-nil brokerTLSCA/
// brokerTLSAddr straight through into ContainerBackendOptions.BrokerTLSCA/
// BrokerTLSAddr, the same "override wins, nothing silently dropped"
// contract the other options fields above already have. The addr pointer
// is dereferenced fresh by dispatcher.ContainerBackendBrokerTLS (the same
// late-binding indirection containerBackend.Launch itself uses) — this
// test writes into the pointed-at string AFTER calling
// sandboxBackendForConfig, mirroring how Server.Start actually populates
// srv.brokerTLSSandboxAddr only after buildRuntime (and so this call) has
// already run, to confirm the wiring really is late-bound and not resolved
// once at construction time.
func TestSandboxBackendForConfig_Container_WiresBrokerTLS(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Sandbox.Backend = config.SandboxBackendContainer

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	var addr string

	be, err := sandboxBackendForConfig(cfg, "install-1", t.TempDir(), ca, &addr)
	if err != nil {
		t.Fatalf("sandboxBackendForConfig: %v", err)
	}

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

// TestSandboxBackendForConfig_Userns_BrokerTLSIgnored pins the companion
// non-regression: a userns (default) config must return (nil, nil)
// regardless of brokerTLSCA/brokerTLSAddr being non-nil — the same
// "container backend selection gates everything in this function" contract
// TestSandboxBackendForConfig_Userns_ReturnsNil already pins for the other
// options fields.
func TestSandboxBackendForConfig_Userns_BrokerTLSIgnored(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	addr := "boid-broker:1234"

	be, err := sandboxBackendForConfig(config.DefaultConfig(), "install-1", t.TempDir(), ca, &addr)
	if err != nil {
		t.Fatalf("sandboxBackendForConfig: %v", err)
	}
	if be != nil {
		t.Fatalf("backend = %v, want nil for the default userns config even with brokerTLSCA/brokerTLSAddr set", be)
	}
}
