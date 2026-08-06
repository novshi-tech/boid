package server

import (
	"path/filepath"
	"testing"
)

// This file pins transcriptsDirFor — the persistence fix for "every job
// transcript is lost on host reboot" (hostVisibleRuntimesDirFor's own doc
// comment): transcript.log/diagnostics.json are daemon-authored, not
// bind-mounted into a job container the way spec.json/state.json are, so
// they can live under dataHomeFor(cfg) (the boid_state named volume, same
// home as boid.db) instead of the host tmpfs hostVisibleRuntimesDirFor
// resolves to.

func TestTranscriptsDirFor_UnderDataHome(t *testing.T) {
	cfg := Config{
		DBPath:     "/home/boid/.local/share/boid/boid.db",
		SocketPath: "/run/user/1000/boid.sock",
	}
	got := transcriptsDirFor(cfg)
	want := filepath.Join("/home/boid/.local/share/boid", "runtime-transcripts")
	if got != want {
		t.Errorf("transcriptsDirFor = %q, want %q", got, want)
	}
	// Must differ from hostVisibleRuntimesDirFor's tmpfs root — that
	// divergence is the entire point of this function existing.
	if hv := hostVisibleRuntimesDirFor(cfg); got == hv {
		t.Errorf("transcriptsDirFor = %q, want it to differ from hostVisibleRuntimesDirFor(cfg) = %q (the tmpfs root)", got, hv)
	}
	// Must also differ from runtimesDirFor's own value (same parent dir,
	// different subdir name) — see transcriptsDirFor's own doc comment for
	// why reusing runtimesDirFor's path would conflate two meanings.
	if rd := runtimesDirFor(cfg); got == rd {
		t.Errorf("transcriptsDirFor = %q, want it to differ from runtimesDirFor(cfg) = %q", got, rd)
	}
}

func TestTranscriptsDirFor_EmptyWhenDataHomeEmpty(t *testing.T) {
	got := transcriptsDirFor(Config{})
	if got != "" {
		t.Errorf("transcriptsDirFor(empty config) = %q, want empty", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("firstNonEmpty(a, b) = %q, want %q", got, "a")
	}
	if got := firstNonEmpty("", "b"); got != "b" {
		t.Errorf("firstNonEmpty(\"\", b) = %q, want %q", got, "b")
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty(\"\", \"\") = %q, want empty", got)
	}
}
