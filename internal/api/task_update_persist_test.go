package api

// TestTaskAppServiceUpdateTask_*_PersistsToRealDB pin TaskAppService.
// UpdateTask's persistence against a REAL sqlite DB (db.Open(":memory:") +
// migrate.Apply — the same idiom task_resolve_or_capture_test.go and
// web_management_test.go use; internal/api cannot import testutil here
// because testutil imports internal/server, which imports internal/api —
// that would be a circular import).
//
// The rest of this package's UpdateTask coverage (service_test.go,
// auto_start_test.go) runs against stubTaskStore, which just mutates an
// in-memory *orchestrator.Task and can never catch a bug in the SQL
// UpdateTask actually issues. That gap is exactly how the remote_id /
// project_id / auto_start columns went missing from UPDATE tasks SET ...
// for as long as they did — every existing test asserted on the *returned*
// task object, which TaskAppService.UpdateTask always populates in memory
// regardless of whether the store call underneath persisted anything. These
// tests instead re-fetch through a brand new orchestrator.GetTask call,
// which only sees what actually made it into the row.

import (
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newRealTaskAppService wires a TaskAppService against a real sqlite DB with
// "proj-1" already registered, and hands back the raw TaskRepository so
// tests can create fixture tasks and re-fetch after UpdateTask.
func newRealTaskAppService(t *testing.T) (*TaskAppService, *orchestrator.TaskRepository) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project proj-1: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-2", WorkDir: "/tmp/proj-2"}); err != nil {
		t.Fatalf("create project proj-2: %v", err)
	}

	tasks := orchestrator.NewTaskRepository(d.Conn)
	svc := &TaskAppService{
		Tasks:    tasks,
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Projects: orchestrator.NewProjectRepository(d.Conn),
	}
	return svc, tasks
}

// TestTaskAppServiceUpdateTask_RemoteID_PersistsToRealDB is the exact
// repro reported against khi production data: PATCH {remote_id: "..."}
// returns 200, but the task_collector self-heal loop's subsequent read
// showed the field never actually changed.
func TestTaskAppServiceUpdateTask_RemoteID_PersistsToRealDB(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)

	// card-model-cleanup PR-2: "working" is now a Card-only status (design
	// doc §3.3), and a Card structurally cannot carry Behavior — this fixture
	// only ever needed "a task past creation, not pending", so it moves to
	// the execution-status equivalent (executing) rather than becoming a
	// Card, since RemoteID editing is gated on neither type nor status.
	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Title:     "t",
		Status:    orchestrator.TaskStatusExecuting,
		RemoteID:  "OLD-1",
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	newRemote := "ROOKPF-306"
	if _, err := svc.UpdateTask(task.ID, UpdateTaskRequest{RemoteID: &newRemote}); err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}

	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.RemoteID != "ROOKPF-306" {
		t.Fatalf("RemoteID after re-fetch = %q, want %q", got.RemoteID, "ROOKPF-306")
	}
}

// TestTaskAppServiceUpdateTask_ProjectID_PersistsToRealDB pins project_id,
// which is behind IsPreDispatchEditableStatus and validated against the
// project store (task_service.go), so status must be pre-dispatch here.
func TestTaskAppServiceUpdateTask_ProjectID_PersistsToRealDB(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Title:     "t",
		Status:    orchestrator.TaskStatusPending,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if _, err := svc.UpdateTask(task.ID, UpdateTaskRequest{ProjectID: "proj-2"}); err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}

	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ProjectID != "proj-2" {
		t.Fatalf("ProjectID after re-fetch = %q, want %q", got.ProjectID, "proj-2")
	}
}

// TestTaskAppServiceUpdateTask_Title_ParkedCard_PersistsToRealDB is the
// regression test for PR #987 review's HIGH 7: card machine v2 folds
// captured/triaged into parked as a card's initial/main resting status
// (docs/plans/suggestion-as-state-transition-impl.md §3.5), so
// orchestrator.IsPreDispatchEditableStatus must include parked — v1's
// blanket "parked is set-aside, not editable" exclusion silently locked
// every fresh card's title/project/instructions out of editing (only
// description slipped through, since UpdateTask's status guard covers just
// those three fields) the moment captured/triaged stopped being reachable.
func TestTaskAppServiceUpdateTask_Title_ParkedCard_PersistsToRealDB(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)

	// card-model-cleanup PR-2: parked is a Card-only status now (design doc
	// §3.3), and a Card structurally has no Behavior — dropped rather than
	// migrated to Exec.Behavior.
	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeCard,
		Title:     "original title",
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if _, err := svc.UpdateTask(task.ID, UpdateTaskRequest{Title: "edited while parked"}); err != nil {
		t.Fatalf("UpdateTask() on a parked card must succeed, got error: %v", err)
	}

	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Title != "edited while parked" {
		t.Fatalf("Title after re-fetch = %q, want %q", got.Title, "edited while parked")
	}
}

// TestTaskAppServiceUpdateTask_Title_WorkingCard_StillRejected pins the
// other half of HIGH 7's fix: only parked (not working) joined the editable
// set — a card with specced/dispatched children or manual work underway
// must still reject a title edit, exactly as before.
func TestTaskAppServiceUpdateTask_Title_WorkingCard_StillRejected(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)

	// working is a Card-only status (design doc §3.3); Behavior dropped for
	// the same reason as the parked-card fixture above.
	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeCard,
		Title:     "original title",
		Status:    orchestrator.TaskStatusWorking,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	_, err := svc.UpdateTask(task.ID, UpdateTaskRequest{Title: "should not land"})
	if err == nil {
		t.Fatal("UpdateTask() on a working card: expected rejection, got success")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}

	got, gErr := tasks.GetTask(task.ID)
	if gErr != nil {
		t.Fatalf("GetTask: %v", gErr)
	}
	if got.Title != "original title" {
		t.Fatalf("Title after rejected edit = %q, want unchanged %q", got.Title, "original title")
	}
}

// TestTaskAppServiceUpdateTask_AutoStart_PersistsToRealDB pins auto_start.
// The task starts already executing so the same-request "trigger a start"
// side effect (task.Status == pending) does not fire — isolating whether the
// flag itself is durably written from whether the immediate trigger ran.
func TestTaskAppServiceUpdateTask_AutoStart_PersistsToRealDB(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Title:     "t",
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev", AutoStart: false},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	autoStart := true
	if _, err := svc.UpdateTask(task.ID, UpdateTaskRequest{AutoStart: &autoStart}); err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}

	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Exec == nil || !got.Exec.AutoStart {
		t.Fatal("AutoStart after re-fetch = false, want true")
	}
}

// TestTaskAppServiceUpdateTask_Payload_CardTask_Rejected pins a contract
// change card-model-cleanup PR-2 review flagged as untested: PATCH
// {payload: ...} against a card task now 409s (task_service.go's `if
// task.Exec == nil` guard right before the payload merge) instead of
// silently no-op-merging into a field that, pre-PR-2, existed as a flat
// zero-value Task.Payload on every task regardless of type. A caller (e.g.
// khi, if it had ever written payload to a card — see design doc §5 for the
// contract-change note) now gets a hard 409 instead of a quiet no-op.
func TestTaskAppServiceUpdateTask_Payload_CardTask_Rejected(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeCard,
		Title:     "a card",
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	_, err := svc.UpdateTask(task.ID, UpdateTaskRequest{Payload: []byte(`{"x":1}`)})
	if err == nil {
		t.Fatal("UpdateTask(payload) on a card: expected rejection, got success")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

// TestTaskAppServiceUpdateTask_AutoStart_CardTask_Rejected is
// TestTaskAppServiceUpdateTask_Payload_CardTask_Rejected's sibling for
// auto_start (task_service.go's own `if task.Exec == nil` guard, added
// specifically because — unlike Instructions — auto_start has no
// IsInstructionsEditable-style status gate to lean on instead).
func TestTaskAppServiceUpdateTask_AutoStart_CardTask_Rejected(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeCard,
		Title:     "a card",
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	autoStart := true
	_, err := svc.UpdateTask(task.ID, UpdateTaskRequest{AutoStart: &autoStart})
	if err == nil {
		t.Fatal("UpdateTask(auto_start) on a card: expected rejection, got success")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}
