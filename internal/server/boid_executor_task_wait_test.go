package server

// TestBoidBuiltinExecutor_TaskWait_* pin the sandboxed task_wait builtin op —
// the half of `boid task wait <id>` that a trigger's `run:` command actually
// calls. The point of the op is that a failed round leaves the sandbox as a
// non-zero exit code, so these tests assert on exit codes and stderr rather
// than on the outcome struct (which internal/api/task_wait_test.go already
// covers directly).
//
// Runs against a REAL sqlite DB (newBoidExecutorTestDB, this package's
// precedent — see boid_executor_task_identity_test.go's file-level comment for
// why this package can't use testutil) so the executor → TaskAppService →
// store chain is exercised end to end.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func taskWaitExecutor(t *testing.T) (*boidBuiltinExecutor, db.DBTX) {
	t.Helper()
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	tasks := orchestrator.NewTaskRepository(conn)
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{
			Tasks:            tasks,
			Actions:          tasks,
			Meta:             executorMetaStub{meta: &orchestrator.ProjectMeta{}},
			WaitPollInterval: time.Millisecond,
		},
	}
	return exec, conn
}

func seedTask(t *testing.T, conn db.DBTX, id string, status orchestrator.TaskStatus) {
	t.Helper()
	tasks := orchestrator.NewTaskRepository(conn)
	task := &orchestrator.Task{
		ID:        id,
		ProjectID: "proj-1",
		Title:     "round",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "sweep"},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	task.Status = status
	if err := tasks.UpdateTask(task); err != nil {
		t.Fatalf("update task status: %v", err)
	}
}

// A round that finished is a success — the trigger's exec job exits 0 and
// TriggerLoop.trackFailStreak resets the trigger's streak.
func TestBoidBuiltinExecutor_TaskWait_DoneExitsZero(t *testing.T) {
	exec, conn := taskWaitExecutor(t)
	seedTask(t, conn, "t-done", orchestrator.TaskStatusDone)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskWait, TaskID: "t-done",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "done") {
		t.Errorf("stdout = %q, want it to name the terminal status", resp.Stdout)
	}
}

// The whole reason the op exists: a round that aborted must reach the daemon as
// a non-zero exit so failStreak counts it, with the reason on stderr so
// `boid job log` tells a human which half broke.
func TestBoidBuiltinExecutor_TaskWait_AbortedExitsNonZeroWithReason(t *testing.T) {
	exec, conn := taskWaitExecutor(t)
	seedTask(t, conn, "t-aborted", orchestrator.TaskStatusAborted)
	payload, err := json.Marshal(map[string]string{"code": "dispatch_error", "message": "harness が起動できなかった"})
	if err != nil {
		t.Fatalf("marshal abort payload: %v", err)
	}
	if err := orchestrator.CreateAction(context.Background(), conn, &orchestrator.Action{
		TaskID:   "t-aborted",
		Type:     "abort",
		ToStatus: orchestrator.TaskStatusAborted,
		Payload:  payload,
		Actor:    orchestrator.ActorDaemon,
	}, nil); err != nil {
		t.Fatalf("create abort action: %v", err)
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskWait, TaskID: "t-aborted",
	})
	if resp.ExitCode == 0 {
		t.Fatalf("exit code = 0, want non-zero; stdout: %s", resp.Stdout)
	}
	if !strings.Contains(resp.Stderr, "dispatch_error") {
		t.Errorf("stderr = %q, want it to carry the abort code", resp.Stderr)
	}
}

// Waiting on the calling task never returns — this job IS why the task has not
// finished. Fail loudly instead of holding the connection until something else
// kills it.
func TestBoidBuiltinExecutor_TaskWait_RejectsWaitingOnItself(t *testing.T) {
	exec, conn := taskWaitExecutor(t)
	seedTask(t, conn, "t-self", orchestrator.TaskStatusExecuting)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}, TaskID: "t-self"}

	// Bounded: without the guard this call blocks forever, and an unbounded
	// context would take the whole package down on the Go test timeout rather
	// than naming the missing guard.
	goCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp := exec.ExecuteBoidBuiltin(goCtx, ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskWait, TaskID: "t-self",
	})
	if resp.ExitCode == 0 {
		t.Fatal("waiting on the calling task should be rejected")
	}
	if !strings.Contains(resp.Stderr, "calling task") {
		t.Errorf("stderr = %q, want it to explain the self-wait", resp.Stderr)
	}
}

// Same workspace scoping as every other task op: a task outside the caller's
// allowed projects is refused before the wait starts.
func TestBoidBuiltinExecutor_TaskWait_RejectsTaskOutsideWorkspace(t *testing.T) {
	exec, conn := taskWaitExecutor(t)
	seedTask(t, conn, "t-other", orchestrator.TaskStatusExecuting)
	ctx := sandbox.TokenContext{ProjectID: "proj-2", AllowedProjectIDs: []string{"proj-2"}}

	goCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp := exec.ExecuteBoidBuiltin(goCtx, ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskWait, TaskID: "t-other",
	})
	if resp.ExitCode == 0 {
		t.Fatal("waiting on a task outside the workspace should be rejected")
	}
	if !strings.Contains(resp.Stderr, "workspace") {
		t.Errorf("stderr = %q, want the workspace-scope rejection", resp.Stderr)
	}
}

// Cancellation (daemon shutdown / sandbox disconnect) ends the wait as an
// error rather than a success — a round whose outcome was never observed must
// not look like one that finished.
func TestBoidBuiltinExecutor_TaskWait_CancelledContextIsNotSuccess(t *testing.T) {
	exec, conn := taskWaitExecutor(t)
	seedTask(t, conn, "t-running", orchestrator.TaskStatusExecuting)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	goCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	resp := exec.ExecuteBoidBuiltin(goCtx, ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskWait, TaskID: "t-running",
	})
	if resp.ExitCode == 0 {
		t.Fatal("a cancelled wait must not report success")
	}
}

// The self-wait guard must compare the RESOLVED id. orchestrator.GetTask falls
// back to a `id LIKE <prefix>%` match for anything >= 8 chars, so passing the
// first 8 characters of the calling task's own id resolves to that very task
// while the raw strings differ — comparing req.TaskID lets the job block on
// itself forever, which is the deadlock the guard exists to prevent. Prefixes
// are ordinary input here (`boid task show <prefix>` works), so this is
// reachable by accident, not only by attack.
func TestBoidBuiltinExecutor_TaskWait_SelfWaitGuardResistsAnIDPrefix(t *testing.T) {
	exec, conn := taskWaitExecutor(t)
	const fullID = "abcdef12-3456-7890-abcd-ef1234567890"
	seedTask(t, conn, fullID, orchestrator.TaskStatusExecuting)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}, TaskID: fullID}

	goCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp := exec.ExecuteBoidBuiltin(goCtx, ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskWait, TaskID: "abcdef12",
	})
	if resp.ExitCode == 0 {
		t.Fatal("waiting on the calling task by id prefix should be rejected")
	}
	if !strings.Contains(resp.Stderr, "calling task") {
		t.Errorf("stderr = %q, want the self-wait rejection (got a timeout instead?)", resp.Stderr)
	}
}

// A round reported on stdout/stderr must name the resolved id, not whatever
// prefix the caller happened to type — the line is what a human reads in
// `boid job log` to find the task.
func TestBoidBuiltinExecutor_TaskWait_ReportsTheResolvedID(t *testing.T) {
	exec, conn := taskWaitExecutor(t)
	const fullID = "fedcba98-7654-3210-fedc-ba9876543210"
	seedTask(t, conn, fullID, orchestrator.TaskStatusDone)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskWait, TaskID: "fedcba98",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, fullID) {
		t.Errorf("stdout = %q, want the resolved id", resp.Stdout)
	}
}
