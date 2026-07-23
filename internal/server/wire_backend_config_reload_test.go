package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins [Major 11, PR7 codex review]: buildRuntime's sandbox
// backend selection config.Load() call must fail daemon startup hard, not
// silently fall back to the userns backend, when config.yaml itself cannot
// be read/parsed at reload time. An operator who wrote `sandbox.backend:
// container` into config.yaml has opted into the container backend as a
// real production dispatch path; a torn config.yaml must not silently
// downgrade that to the pre-Phase-6 userns backend with no visible error.

// TestNew_SandboxBackendConfigLoadFailure_RefusesStartup pins the fix: an
// unreadable-as-YAML config.yaml (present, but invalid — config.Load's own
// loadFromPath only treats ENOENT as "use defaults", any other read/parse
// failure is a hard error) makes New() return an error, not a daemon that
// silently started with the userns backend.
func TestNew_SandboxBackendConfigLoadFailure_RefusesStartup(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	boidConfigDir := filepath.Join(configHome, "boid")
	if err := os.MkdirAll(boidConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	// Invalid YAML (a bare scalar where the top-level document must be a
	// mapping) — os.ReadFile succeeds, yaml.Unmarshal fails, matching
	// config.loadFromPath's "any other error" (non-ENOENT) branch.
	invalidYAML := "not: [valid: yaml: at: all\n"
	if err := os.WriteFile(filepath.Join(boidConfigDir, "config.yaml"), []byte(invalidYAML), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	_, err := New(Config{DBPath: ":memory:", SocketPath: filepath.Join(t.TempDir(), "boid.sock")})
	if err == nil {
		t.Fatal("New() = nil error, want daemon startup refused (config.yaml is unreadable)")
	}
	if !strings.Contains(err.Error(), "sandbox backend selection") {
		t.Errorf("New() error = %q, want it to mention sandbox backend selection config load", err.Error())
	}
}

// TestNew_SandboxBackendConfigMissing_StartsWithUserns pins the companion
// non-regression: a MISSING config.yaml (the common case — no file at all)
// is not a load failure at all (config.Load's own ENOENT branch returns
// DefaultConfig(), nil), so New() must still start normally with the
// default userns backend.
func TestNew_SandboxBackendConfigMissing_StartsWithUserns(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, err := New(Config{DBPath: ":memory:", SocketPath: filepath.Join(t.TempDir(), "boid.sock")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv == nil {
		t.Fatal("New() returned a nil server with a nil error")
	}
}
