package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
)

// TestServer_New_RunnerDataHomeDirIsPersistentDataRoot pins the server-side
// half of the ONE new wiring edge PR2 of
// docs/plans/workspace-home-volume-persistence.md introduces (論点b):
// dataHomeFor(cfg) -> dispatcher.WireConfig.DataHomeDir -> Runner.DataHomeDir,
// which is where resolveWorkspaceHome now writes the init completion marker
// and takes the init lock (homes-meta/<slug>.{init.json,lock}).
//
// The dispatcher-side half — Runner.DataHomeDir actually reaching those two
// files on disk — is pinned by TestWire_DataHomeDir_ReachesMarkerAndLockOnDisk
// (internal/dispatcher/workspace_home_test.go). This test covers the other
// end: buildRuntime dropping the field, or passing runtimesRoot for it, would
// leave that test green while putting the marker and lock straight back onto
// tmpfs — the exact persistence regression this whole plan doc exists to fix.
//
// Same Server.New() -> buildRuntime() approach, and the same deliberately
// split DBPath/SocketPath parents, as
// TestServer_New_TaskAppServiceUsesHostVisibleRuntimesDir — see that test's
// own comment for why the two roots must not coincide here.
func TestServer_New_RunnerDataHomeDirIsPersistentDataRoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

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

	// The *dispatcher.Runner buildRuntime built is not stored on Server
	// directly; the boid builtin executor's jobContexts field is the one
	// post-construction handle on it (same reach-through as
	// TestServer_New_TaskAppServiceUsesHostVisibleRuntimesDir).
	if srv.broker == nil || srv.broker.BoidExecutor == nil {
		t.Fatal("srv.broker.BoidExecutor is nil, want the wired boidBuiltinExecutor")
	}
	exec, ok := srv.broker.BoidExecutor.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("srv.broker.BoidExecutor is %T, want *boidBuiltinExecutor", srv.broker.BoidExecutor)
	}
	runner, ok := exec.jobContexts.(*dispatcher.Runner)
	if !ok {
		t.Fatalf("boidBuiltinExecutor.jobContexts is %T, want *dispatcher.Runner", exec.jobContexts)
	}

	want := dataHomeFor(cfg)
	if want == "" {
		t.Fatal("test setup bug: dataHomeFor(cfg) is empty")
	}
	if runner.DataHomeDir != want {
		t.Errorf("Runner.DataHomeDir = %q, want %q (dataHomeFor(cfg) — the same root boid.db/web_secret/install_id live in)",
			runner.DataHomeDir, want)
	}
	if bad := hostVisibleRuntimesDirFor(cfg); runner.DataHomeDir == bad {
		t.Errorf("Runner.DataHomeDir = %q equals hostVisibleRuntimesDirFor(cfg) — that root is BOID_RUNTIME_DIR (tmpfs), which is exactly where the marker/lock must NOT live", runner.DataHomeDir)
	}
}
