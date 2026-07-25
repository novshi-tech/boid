package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins [Major 11, PR7 codex review, still relevant post-PR-4]:
// buildRuntime's config.Load() call must fail daemon startup hard, not
// silently proceed on defaults, when config.yaml itself cannot be
// read/parsed at reload time.

// TestNew_ConfigLoadFailure_RefusesStartup pins the fix: an
// unreadable-as-YAML config.yaml (present, but invalid — config.Load's own
// loadFromPath only treats ENOENT as "use defaults", any other read/parse
// failure is a hard error) makes New() return an error, not a daemon that
// silently started on defaults.
//
// PR-4 (docs/plans/volume-only-daemon.md §論点e) removed the userns
// backend and the sandbox.backend config-driven branch this check used to
// specifically guard ("must not silently downgrade to userns") — the
// invariant itself ("config.yaml must actually be loadable at boot")
// stands on its own regardless, so the check (and this test) stay, just
// without the userns-specific framing.
func TestNew_ConfigLoadFailure_RefusesStartup(t *testing.T) {
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
	if !strings.Contains(err.Error(), "load boid config") {
		t.Errorf("New() error = %q, want it to mention the config load failure", err.Error())
	}
}

// TestNew_ConfigMissing_StartsNormally pins the companion non-regression: a
// MISSING config.yaml (the common case — no file at all) is not a load
// failure at all (config.Load's own ENOENT branch returns DefaultConfig(),
// nil), so New() must still start normally.
func TestNew_ConfigMissing_StartsNormally(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, err := New(Config{DBPath: ":memory:", SocketPath: filepath.Join(t.TempDir(), "boid.sock")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv == nil {
		t.Fatal("New() returned a nil server with a nil error")
	}
}
