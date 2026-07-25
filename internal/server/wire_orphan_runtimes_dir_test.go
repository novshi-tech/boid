package server

import (
	"os"
	"path/filepath"
	"testing"
)

// TestServer_New_ContainerBackend_CleanOrphanRuntimesScansHostVisibleDir
// pins PR834 PR-2b round-2 codex review Major 2: buildRuntime's startup
// orphan-runtime-dir cleanup (wire.go's cleanOrphanRuntimes call) used to
// scan runtimesDirFor(cfg) — the boid_state NAMED VOLUME root, invisible to
// a DooD sibling job container's own host-side bind-mount source — even
// under the container backend, where every actual PR-2b artifact (per-job
// clone staging, workspace HOME, transcripts) lands under
// hostVisibleRuntimesDirFor(cfg) instead (runner.RuntimesDir,
// sandboxBackendForConfig's runtimeDir argument, transcriptLogReader — see
// TestServer_New_ContainerBackend_TaskAppServiceUsesHostVisibleRuntimesDir
// for the sibling wiring-seam bug this same class of divergence caused).
// Left unfixed, a daemon crash mid-dispatch under container backend leaves
// an orphaned per-job clone staging dir under the HOST-VISIBLE root that
// startup cleanup never even looks at — it accumulates until the 30-day GC
// threshold instead of being reclaimed on the very next daemon start the
// way a userns deployment's orphans already are.
//
// Goes through the real Server.New() -> buildRuntime() wiring (same pattern
// as wire_task_runtimes_dir_test.go) rather than calling cleanOrphanRuntimes
// directly: the bug is specifically about WHICH root buildRuntime passes it,
// not about cleanOrphanRuntimes' own logic (already covered by
// wire_gc_test.go).
func TestServer_New_ContainerBackend_CleanOrphanRuntimesScansHostVisibleDir(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	boidConfigDir := filepath.Join(configHome, "boid")
	if err := os.MkdirAll(boidConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidConfigDir, "config.yaml"), []byte("sandbox:\n  backend: container\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	// DBPath and SocketPath deliberately live under DIFFERENT parent
	// directories, exactly like wire_task_runtimes_dir_test.go's sibling
	// test — otherwise runtimesDirFor(cfg) and hostVisibleRuntimesDirFor(cfg)
	// would coincidentally compute the same path and this test could not
	// distinguish the bug from a correct fix.
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	runtimeDir := filepath.Join(tmpDir, "runtime")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	cfg := Config{
		DBPath:     filepath.Join(dataDir, "boid.db"),
		SocketPath: filepath.Join(runtimeDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		JobRuntime: &fakeJobRuntime{},
	}

	// Plant an orphan runtime dir (no corresponding job row — the DB is
	// freshly created below, so any entry here is orphaned by definition)
	// under the HOST-VISIBLE root, where a container-backend deploy's own
	// per-job clone staging area actually lands.
	hostVisibleRoot := hostVisibleRuntimesDirFor(cfg)
	orphanDir := filepath.Join(hostVisibleRoot, "orphan-runtime-id")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "marker.txt"), []byte("leftover from a crashed dispatch\n"), 0o644); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if _, statErr := os.Stat(orphanDir); !os.IsNotExist(statErr) {
		t.Errorf("expected orphan dir %s (under hostVisibleRuntimesDirFor(cfg)) to be cleaned up on startup under container backend, stat err = %v", orphanDir, statErr)
	}
}
