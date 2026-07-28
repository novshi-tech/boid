package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/novshi-tech/boid/testutil"
)

// noopBackend is a backend.SandboxBackend that starts jobs but never
// auto-completes them, allowing tests to manually complete jobs via the
// API. Replaces the pre-PR-4 noopRuntime (a dispatcher.JobRuntime fake —
// docs/plans/volume-only-daemon.md §論点e removed that userns-only seam).
// Reused across this package's other server_test-package smoke/gateway
// test files via server.Config.Backend, the successor to their own
// pre-PR-4 Config.JobRuntime usage.
type noopBackend struct{}

var _ backend.SandboxBackend = (*noopBackend)(nil)

func (noopBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return noopSession{}, nil
}
func (noopBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}

// EnsureWorkspaceHomeVolume and RunWorkspaceInit satisfy
// dispatcher.WorkspaceInitExecutor — required of every backend since PR5/PR6 of
// docs/plans/workspace-home-volume-persistence.md, which moved workspace home
// preparation into a throwaway container the backend owns and the home itself
// into a docker named volume. See workspace_init_stub_test.go.
// (Duplicated rather than shared with workspace_init_stub_test.go: this file is
// package server_test, that one is package server.)
func (noopBackend) EnsureWorkspaceHomeVolume(_ context.Context, req dispatcher.WorkspaceHomeVolumeRequest) (string, error) {
	dir := noopBackendVolumeDir(req.Name)
	idPath := dir + ".id"
	if data, err := os.ReadFile(idPath); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(idPath, []byte(req.CandidateID), 0o600); err != nil {
		return "", err
	}
	return req.CandidateID, nil
}

// noopBackendVolumeDir models a docker volume as a directory under whatever
// throwaway XDG_DATA_HOME testutil/homeenv established, so it is removed with
// it and no two tests share one.
func noopBackendVolumeDir(name string) string {
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, "boid-test-volumes", name)
}

func (noopBackend) RunWorkspaceInit(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	homeDir := noopBackendVolumeDir(req.HomeSource)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return err
	}
	env := make([]string, 0, len(req.Env)+1)
	for k, v := range req.Env {
		if v == req.HomeTarget {
			v = homeDir
		}
		env = append(env, k+"="+v)
	}
	env = append(env, "PATH="+os.Getenv("PATH"))
	cmd := exec.CommandContext(ctx, "/bin/bash", "-s")
	cmd.Stdin = strings.NewReader(req.Script)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace init (test stand-in) failed: %w\n%s", err, out)
	}
	return nil
}

func (noopBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

type noopSession struct{}

var _ backend.SandboxSession = noopSession{}

func (noopSession) ID() string { return "noop-runtime" }
func (noopSession) Subscribe() ([]byte, <-chan []byte, func(), bool, bool) {
	return nil, nil, func() {}, false, true
}
func (noopSession) WriteInput([]byte) error           { return dispatcher.ErrRuntimeUnsupported }
func (noopSession) CloseInput() error                 { return dispatcher.ErrRuntimeUnsupported }
func (noopSession) Resize(backend.TerminalSize) error { return dispatcher.ErrRuntimeUnsupported }
func (noopSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return backend.RuntimeExit{}, dispatcher.ErrRuntimeUnsupported
}
func (noopSession) Stop(context.Context) error                   { return nil }
func (noopSession) Signal(context.Context, syscall.Signal) error { return nil }

func TestServer_Smoke_StartDispatchJobDoneAndAutoAdvance(t *testing.T) {
	ts := newSmokeServer(t)
	projectDir := writeSmokeProject(t)

	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]any{
		"work_dir": projectDir,
	}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if project.ID != "smoke-proj" {
		t.Fatalf("project id = %q, want %q", project.ID, "smoke-proj")
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": project.ID,
		"title":      "Smoke",
		"behavior":   "impl",
		"payload":    map[string]any{},
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.Status != orchestrator.TaskStatusPending {
		t.Fatalf("initial task status = %q, want %q", task.Status, orchestrator.TaskStatusPending)
	}

	var applied struct {
		Task orchestrator.Task `json:"task"`
	}
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/actions", map[string]any{
		"type": "start",
	}, &applied); err != nil {
		t.Fatalf("start action: %v", err)
	}
	if applied.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("started task status = %q, want %q", applied.Task.Status, orchestrator.TaskStatusExecuting)
	}

	job := waitForSingleJob(t, ts, task.ID)
	if job.Role != "hook" {
		t.Fatalf("job role = %q, want hook", job.Role)
	}

	if err := ts.Client.Do("POST", "/api/jobs/"+job.ID+"/done", map[string]any{
		"exit_code": 0,
		"output":    `{"payload_patch":{"artifact":{"pr_url":"https://example.com/pr/1"}}}`,
	}, &job); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	finalTask := waitForTaskStatus(t, ts, task.ID, orchestrator.TaskStatusDone)

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(finalTask.Payload, &payload); err != nil {
		t.Fatalf("unmarshal final payload: %v", err)
	}
	artifact, ok := payload["artifact"]
	if !ok || string(artifact) == "null" {
		t.Fatalf("final payload missing artifact: %s", finalTask.Payload)
	}
}

func TestServer_TaskDetailIncludesActionsAndJobs(t *testing.T) {
	ts := newSmokeServer(t)
	projectDir := writeSmokeProject(t)

	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]any{
		"work_dir": projectDir,
	}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": project.ID,
		"title":      "Smoke",
		"behavior":   "impl",
		"payload":    map[string]any{"prompt": "test"},
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/actions", map[string]any{
		"type": "start",
	}, nil); err != nil {
		t.Fatalf("start action: %v", err)
	}

	job := waitForSingleJob(t, ts, task.ID)

	var detail struct {
		Task    orchestrator.Task     `json:"task"`
		Actions []orchestrator.Action `json:"actions"`
		Jobs    []jobView             `json:"jobs"`
	}
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID+"/detail", nil, &detail); err != nil {
		t.Fatalf("get task detail: %v", err)
	}

	if detail.Task.ID != task.ID {
		t.Fatalf("task detail id = %q, want %q", detail.Task.ID, task.ID)
	}
	if len(detail.Actions) != 1 || detail.Actions[0].Type != "start" {
		t.Fatalf("actions = %+v, want single start action", detail.Actions)
	}
	if len(detail.Jobs) != 1 || detail.Jobs[0].ID != job.ID {
		t.Fatalf("jobs = %+v, want job %q", detail.Jobs, job.ID)
	}
}

func newSmokeServer(t *testing.T) *testutil.TestServer {
	t.Helper()

	// Isolate from the real $XDG_CONFIG_HOME (see testutil.NewTestServer's
	// doc comment) — this helper constructs *server.Server directly rather
	// than via testutil.NewTestServer.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")
	dbPath := filepath.Join(tmpDir, "boid.db")

	srv, err := server.New(server.Config{
		DBPath:     dbPath,
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		Backend:    &noopBackend{},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	return &testutil.TestServer{
		Server: srv,
		Client: client.NewUnixClient(sockPath),
	}
}

func writeSmokeProject(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	testutil.InitGitRepoWithOrigin(t, dir)
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid: %v", err)
	}

	// Behavior-level kits are no longer supported in project.yaml; hooks are
	// now supplied by workspace kits. Use default_instruction so that start
	// synthesises a virtual agent-kind hook job for the executor agent.
	projectYAML := `id: smoke-proj
name: Smoke Project
task_behaviors:
  impl:
    name: implementation
    default_instruction:
      type: execution
      agent: claude-code
      message: smoke test
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}

	return dir
}

func waitForSingleJob(t *testing.T, ts *testutil.TestServer, taskID string) jobView {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var jobs []jobView
		if err := ts.Client.Do("GET", "/api/jobs?task_id="+taskID, nil, &jobs); err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		if len(jobs) == 1 {
			return jobs[0]
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for dispatched job")
	return jobView{}
}

func waitForTaskStatus(t *testing.T, ts *testutil.TestServer, taskID string, want orchestrator.TaskStatus) orchestrator.Task {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var task orchestrator.Task
		if err := ts.Client.Do("GET", "/api/tasks/"+taskID, nil, &task); err != nil {
			t.Fatalf("get task: %v", err)
		}
		if task.Status == want {
			return task
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for task %s to reach status %q", taskID, want)
	return orchestrator.Task{}
}

type jobView struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Role   string `json:"role"`
	Status string `json:"status"`
}
