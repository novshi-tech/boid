package server

import (
	"testing"

	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
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
	be, err := sandboxBackendForConfig(config.DefaultConfig(), "install-1", t.TempDir())
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
	be, err := sandboxBackendForConfig(nil, "install-1", t.TempDir())
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

	be, err := sandboxBackendForConfig(cfg, "install-1", t.TempDir())
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
