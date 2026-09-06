package orchestrator_test

// Pins orchestrator.TaskRepository.CreateTaskLinkedToCardRequest — the
// atomic task-insert + request-attach a card-command launcher's `boid task
// create` relies on, so a task continuation is never observable with no
// request association.

import (
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestCreateTaskLinkedToCardRequest_AttachesInOneCall(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	repo := orchestrator.NewTaskRepository(d.Conn)

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Exec:      &orchestrator.ExecAttrs{Behavior: "executor"},
	}
	if err := repo.CreateTaskLinkedToCardRequest(task, req.ID, "job-1"); err != nil {
		t.Fatalf("CreateTaskLinkedToCardRequest: %v", err)
	}
	if task.ID == "" {
		t.Fatal("task.ID was not assigned")
	}

	got, err := repo.GetCardRequest(req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached {
		t.Errorf("status = %q, want attached", got.Status)
	}
	if got.TargetKind != orchestrator.CardRequestTargetKindTask || got.TargetID != task.ID {
		t.Errorf("target = %s/%s, want task/%s", got.TargetKind, got.TargetID, task.ID)
	}
}

// TestCreateTaskLinkedToCardRequest_RetrySameRefConverges pins the
// idempotent-retry case: a second call with the SAME Ref (so CreateTask's
// own get-or-create returns the existing task, not a new row) must succeed
// rather than erroring on AttachCardRequest's "already attached" — a
// retried launcher must converge, not fail.
func TestCreateTaskLinkedToCardRequest_RetrySameRefConverges(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	repo := orchestrator.NewTaskRepository(d.Conn)

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}

	first := &orchestrator.Task{ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Ref: req.ID, Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	if err := repo.CreateTaskLinkedToCardRequest(first, req.ID, "job-1"); err != nil {
		t.Fatalf("first CreateTaskLinkedToCardRequest: %v", err)
	}

	retry := &orchestrator.Task{ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Ref: req.ID, Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	if err := repo.CreateTaskLinkedToCardRequest(retry, req.ID, "job-1"); err != nil {
		t.Fatalf("retry CreateTaskLinkedToCardRequest: %v", err)
	}
	if retry.ID != first.ID {
		t.Errorf("retry.ID = %q, want the same task id %q as the first call (Ref get-or-create)", retry.ID, first.ID)
	}

	execTasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ProjectID: "proj-1", Behavior: "executor"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(execTasks) != 1 {
		t.Fatalf("execution tasks = %d, want exactly 1 (no duplicate planted by the retry)", len(execTasks))
	}
}

// TestCreateTaskLinkedToCardRequest_AlreadyAttachedToDifferentTask_Errors
// pins the conflicting case: if the request is already attached to a
// DIFFERENT task than the one this call just created/found, that is a real
// error, not silently swallowed.
func TestCreateTaskLinkedToCardRequest_AlreadyAttachedToDifferentTask_Errors(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	repo := orchestrator.NewTaskRepository(d.Conn)

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "some-other-task-id"); err != nil {
		t.Fatalf("AttachCardRequest: %v", err)
	}

	task := &orchestrator.Task{ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Ref: "brand-new-ref", Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	err := repo.CreateTaskLinkedToCardRequest(task, req.ID, "job-1")
	if !errors.Is(err, orchestrator.ErrCardRequestInvalidTransition) {
		t.Fatalf("err = %v, want ErrCardRequestInvalidTransition", err)
	}
}

// TestCreateTaskLinkedToCardRequest_AttachFailure_RollsBackTheTaskInsert pins
// CreateTaskLinkedToCardRequest's atomicity: when AttachCardRequest fails,
// the task INSERT from the SAME call must not survive either — the two
// writes are one transaction, not "create, then best-effort attach".
func TestCreateTaskLinkedToCardRequest_AttachFailure_RollsBackTheTaskInsert(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	repo := orchestrator.NewTaskRepository(d.Conn)

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "some-other-task-id"); err != nil {
		t.Fatalf("AttachCardRequest: %v", err)
	}

	task := &orchestrator.Task{ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Ref: "brand-new-ref", Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	if err := repo.CreateTaskLinkedToCardRequest(task, req.ID, "job-1"); !errors.Is(err, orchestrator.ErrCardRequestInvalidTransition) {
		t.Fatalf("err = %v, want ErrCardRequestInvalidTransition", err)
	}

	execTasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ProjectID: "proj-1", Behavior: "executor"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, et := range execTasks {
		if et.Ref == "brand-new-ref" {
			t.Fatalf("found task %q with Ref=brand-new-ref — the task INSERT must have rolled back alongside the failed attach, not survived it", et.ID)
		}
	}
}
