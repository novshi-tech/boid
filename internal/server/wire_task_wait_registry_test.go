package server

import (
	"os"
	"path/filepath"
	"testing"
)

// TestServer_New_TaskWaitRegistryIsSharedByBothServices pins a two-ended wiring
// seam: `boid task wait` records which task a job is parked on (through
// TaskAppService, reached from the brokered op), and the trigger sweep reads
// that to know WHAT to end when a round outruns its `timeout` (through
// TaskWorkflowService). They must hold the SAME *api.TaskWaitRegistry.
//
// Wired to two different registries — or to one and a nil — nothing fails, no
// test breaks, and every timed-out round quietly stops only its job while the
// task it started keeps running: single-flight is released and the next tick
// begins a second concurrent round of the same work, which is the exact failure
// the registry exists to prevent. There is a runtime slog.Warn for the nil case,
// but it only fires when a round actually times out, possibly months later, in a
// log nobody is reading.
//
// Reaches taskSvc the way wire_task_runtimes_dir_test.go documents — via
// srv.broker.BoidExecutor, since New() never stores the *appRuntime it built —
// and the workflow service via srv.workflow.
func TestServer_New_TaskWaitRegistryIsSharedByBothServices(t *testing.T) {
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

	if srv.workflow == nil {
		t.Fatal("srv.workflow is nil")
	}
	exec, ok := srv.broker.BoidExecutor.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("srv.broker.BoidExecutor is %T, want *boidBuiltinExecutor", srv.broker.BoidExecutor)
	}
	if exec.tasks == nil {
		t.Fatal("boidBuiltinExecutor.tasks (*api.TaskAppService) is nil")
	}

	if srv.workflow.TaskWaits == nil {
		t.Fatal("TaskWorkflowService.TaskWaits is nil — a timed-out round could only stop its job, not the task")
	}
	if exec.tasks.TaskWaits == nil {
		t.Fatal("TaskAppService.TaskWaits is nil — `boid task wait` would record nothing for the sweep to read")
	}
	if srv.workflow.TaskWaits != exec.tasks.TaskWaits {
		t.Error("the two services hold DIFFERENT TaskWaitRegistry instances — what the wait records is invisible to the sweep that reads it")
	}
}
