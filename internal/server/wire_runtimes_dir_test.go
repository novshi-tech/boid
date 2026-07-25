package server

import (
	"path/filepath"
	"testing"
)

// This file pins hostVisibleRuntimesDirFor (docs/plans/volume-only-daemon.md
// — the fix for the gap build/container/compose.yml's own "KNOWN GAP"
// comment flags): under the volume-only compose deploy, runtimesDirFor(cfg)
// resolves under BOID_DATA_DIR (the boid_state NAMED VOLUME, invisible to a
// DooD sibling container's host-side bind-mount source), so container
// backend call sites must use this function instead — see its own doc
// comment for the full rationale.

func TestHostVisibleRuntimesDirFor_PrefersSocketPathDir(t *testing.T) {
	cfg := Config{
		DBPath:     "/home/boid/.local/share/boid/boid.db",
		SocketPath: "/run/user/1000/boid.sock",
	}
	got := hostVisibleRuntimesDirFor(cfg)
	want := filepath.Join("/run/user/1000", "runtimes")
	if got != want {
		t.Errorf("hostVisibleRuntimesDirFor = %q, want %q", got, want)
	}
	// Must differ from the data-dir-based root — that divergence IS the fix.
	if dataDirRoot := runtimesDirFor(cfg); got == dataDirRoot {
		t.Errorf("hostVisibleRuntimesDirFor = %q, want it to differ from runtimesDirFor(cfg) = %q (the named-volume-based root)", got, dataDirRoot)
	}
}

func TestHostVisibleRuntimesDirFor_FallsBackWhenSocketPathEmpty(t *testing.T) {
	cfg := Config{DBPath: "/home/boid/.local/share/boid/boid.db"}
	got := hostVisibleRuntimesDirFor(cfg)
	want := runtimesDirFor(cfg)
	if got != want {
		t.Errorf("hostVisibleRuntimesDirFor = %q, want fallback to runtimesDirFor(cfg) = %q when SocketPath is empty", got, want)
	}
}

// TestRuntimesDirFor_UnaffectedByHostVisibleVariant pins that the
// PRE-EXISTING runtimesDirFor (used for the userns backend's LocalRuntime
// root, GC of orphan runtime dirs, and every non-container-backend
// deployment) stays byte-for-byte unchanged — this PR adds a new function
// rather than modifying this one, precisely so a userns deployment (still
// the overwhelming majority of installs) is completely unaffected.
func TestRuntimesDirFor_UnaffectedByHostVisibleVariant(t *testing.T) {
	cfg := Config{
		DBPath:     "/home/user/.local/share/boid/boid.db",
		SocketPath: "/run/user/1000/boid.sock",
	}
	got := runtimesDirFor(cfg)
	want := filepath.Join("/home/user/.local/share/boid", "runtimes")
	if got != want {
		t.Errorf("runtimesDirFor = %q, want %q (DBPath-derived, unchanged)", got, want)
	}
}
