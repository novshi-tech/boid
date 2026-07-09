package server_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/novshi-tech/boid/testutil"
)

// noopRuntime is a JobRuntime that starts jobs but never auto-completes them,
// allowing tests to manually complete jobs via the API.
type noopRuntime struct{}

func (noopRuntime) Start(_ context.Context, _ dispatcher.RuntimeStartSpec) (*dispatcher.RuntimeHandle, error) {
	return &dispatcher.RuntimeHandle{ID: "noop-runtime"}, nil
}
func (noopRuntime) Attach(_ context.Context, _ string, _ dispatcher.RuntimeAttachRequest) error {
	return dispatcher.ErrRuntimeUnsupported
}
func (noopRuntime) Resize(_ context.Context, _ string, _ dispatcher.TerminalSize) error {
	return dispatcher.ErrRuntimeUnsupported
}
func (noopRuntime) Wait(_ context.Context, _ string) (dispatcher.RuntimeExit, error) {
	return dispatcher.RuntimeExit{}, dispatcher.ErrRuntimeUnsupported
}
func (noopRuntime) Stop(_ context.Context, _ string) error { return nil }
func (noopRuntime) Signal(_ context.Context, _ string, _ syscall.Signal) error {
	return nil
}

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

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")
	dbPath := filepath.Join(tmpDir, "boid.db")

	srv, err := server.New(server.Config{
		DBPath:     dbPath,
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		JobRuntime: noopRuntime{},
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
