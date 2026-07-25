package server

import (
	"os"
	"path/filepath"
	"testing"
)

// TestServer_New_TaskAppServiceUsesHostVisibleRuntimesDir pins codex
// round-1, PR834 Minor 1: buildRuntime's TaskAppService construction
// (wire.go) previously passed a fresh runtimesDirFor(cfg) — the boid_state
// NAMED VOLUME root, invisible on the host under container backend —
// instead of reusing runtimesRoot, the same value every other call site in
// buildRuntime (runner.RuntimesDir, sandboxBackendForConfig's own
// runtimeDir argument, transcriptLogReader.rootDir) already resolves to via
// hostVisibleRuntimesDirFor(cfg). Left unfixed, a task's own
// `workspace_path` (api.enrichJob: filepath.Join(RuntimesDir,
// job.RuntimeID)) would point somewhere nothing on the host can actually
// read — exactly the "one end of a wiring seam changed, the other silently
// drifted" class of bug this test exists to catch.
//
// PR-4 (docs/plans/volume-only-daemon.md §論点e) made hostVisibleRuntimesDirFor
// unconditional (container is the only sandbox backend now, no more
// config-driven userns/container branch) — this test no longer needs to
// seed a config.yaml selecting it.
//
// Goes through the real Server.New() -> buildRuntime() wiring (not a direct
// unit test of runtimesDirFor/hostVisibleRuntimesDirFor in isolation — see
// wire_runtimes_dir_test.go for those two, and
// server_container_backend_gateway_test.go for the equivalent pattern
// applied to GatewayURL). taskSvc itself is reachable post-construction only
// via srv.broker.BoidExecutor (wire.go's newBoidBuiltinExecutor call) — New()
// never stores the *appRuntime it built directly on Server.
func TestServer_New_TaskAppServiceUsesHostVisibleRuntimesDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// DBPath and SocketPath deliberately live under DIFFERENT parent
	// directories — mirroring the real compose deploy this test's own
	// container-backend config represents, where BOID_DATA_DIR (the
	// boid_state NAMED VOLUME, DBPath's parent) and BOID_RUNTIME_DIR (the
	// host BIND mount, SocketPath's parent) are genuinely different roots
	// (build/container/compose.yml's own "Persistence" header comment). If
	// both lived under the SAME tmpDir (as a plain non-container-backend
	// test fixture normally would), runtimesDirFor(cfg) and
	// hostVisibleRuntimesDirFor(cfg) would coincidentally compute the exact
	// same path regardless of which one wire.go's TaskAppService
	// construction actually used, making this test unable to distinguish
	// the codex round-1 Minor 1 regression from a correct fix at all.
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
		Backend:    &fakeSandboxBackend{},
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if srv.broker == nil || srv.broker.BoidExecutor == nil {
		t.Fatal("srv.broker.BoidExecutor is nil, want the wired boidBuiltinExecutor")
	}
	exec, ok := srv.broker.BoidExecutor.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("srv.broker.BoidExecutor is %T, want *boidBuiltinExecutor", srv.broker.BoidExecutor)
	}
	if exec.tasks == nil {
		t.Fatal("boidBuiltinExecutor.tasks (*api.TaskAppService) is nil")
	}

	want := hostVisibleRuntimesDirFor(cfg)
	if exec.tasks.RuntimesDir != want {
		t.Errorf("TaskAppService.RuntimesDir = %q, want %q (hostVisibleRuntimesDirFor(cfg), matching runner.RuntimesDir)",
			exec.tasks.RuntimesDir, want)
	}
	if bad := runtimesDirFor(cfg); exec.tasks.RuntimesDir == bad {
		t.Errorf("TaskAppService.RuntimesDir = %q equals runtimesDirFor(cfg) (the boid_state named-volume root) — under container backend it must be the host-visible root instead", exec.tasks.RuntimesDir)
	}
}
