package api

// docs/plans/ingestion-identity.md PR-3 (B-3): end-to-end (real sqlite,
// db.Open(":memory:") + migrate.Apply — the same idiom
// task_resolve_or_capture_test.go uses, per PR-1/PR-2 review note #3:
// importing testutil here would cycle through internal/server) coverage of
// TaskWorkflowService.ListActions: actions written via a real ApplyAction
// call, read back via ListActions, through the SAME underlying
// orchestrator.TaskRepository — pinning the whole write-then-read path, not
// just the store layer in isolation (action_list_test.go, internal/orchestrator).

import (
	"context"
	"strconv"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newActionListTestService(t *testing.T) (*TaskWorkflowService, *orchestrator.TaskRepository) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, id := range []string{"proj-1", "proj-2"} {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
	}
	taskRepo := orchestrator.NewTaskRepository(d.Conn)
	svc := &TaskWorkflowService{
		Tasks:   taskRepo,
		Actions: taskRepo,
		Tx:      realTransactor{conn: d.Conn},
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			DefaultTaskBehavior: "triage",
			TaskBehaviors:       map[string]orchestrator.TaskBehavior{"triage": {}},
			BaseBranch:          "main",
		}},
		// PR-B (docs/plans/suggestion-as-state-transition-impl.md §2):
		// machineFor needs this to pick NewCardMachine for the parked-status
		// card tasks these tests create — same taskRepo, which already
		// implements CardStore (wire.go wires it identically in
		// production).
		TaskTriage: taskRepo,
	}
	return svc, taskRepo
}

// TestListActions_Unscoped_Rejected pins B-3's own contract at the service
// layer: an ActionListFilter naming neither a project nor a workspace is
// refused rather than silently returning the daemon's ENTIRE action log.
func TestListActions_Unscoped_Rejected(t *testing.T) {
	svc, _ := newActionListTestService(t)
	if _, err := svc.ListActions(orchestrator.ActionListFilter{}); err == nil {
		t.Fatal("ListActions with an unscoped filter = nil error, want a rejection")
	}
}

// TestListActions_NotedRoundTrips writes a noted action through the real
// ApplyAction path and reads it back through ListActions — the doc's own
// "noted が任意形状の JSON payload を通し、action_list でそのまま返る"
// verification item, exercised end to end rather than at either layer alone.
func TestListActions_NotedRoundTrips(t *testing.T) {
	svc, taskRepo := newActionListTestService(t)
	// card-model-cleanup PR-2: a card's kind/urgency/wake_at/... columns now
	// live directly on the tasks row (migration 0045) — CreateTask with
	// Type=Card + a non-nil Card already populates them, so there is no more
	// separate SeedTaskTriage step (the function/method is gone entirely).
	// Without Type=Card, machineFor would fall back to NewExecutionMachine,
	// which has no "noted" rule at all.
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	if err := taskRepo.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "noted",
		Payload: []byte(`{"kind":"suggestion_reviewed","cycle":3}`),
	}); err != nil {
		t.Fatalf("ApplyAction(noted): %v", err)
	}

	result, err := svc.ListActions(orchestrator.ActionListFilter{ProjectIDs: []string{"proj-1"}})
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(result.Actions))
	}
	if string(result.Actions[0].Payload) != `{"kind":"suggestion_reviewed","cycle":3}` {
		t.Errorf("payload = %s, want it returned verbatim", result.Actions[0].Payload)
	}
	if result.NextCursor == "" {
		t.Error("expected a non-empty next cursor after a non-empty page")
	}
}

// TestListActions_ProjectScoping_NeverLeaksOtherProject pins scoping at the
// full service-method level: a caller scoped to proj-1 must never see
// proj-2's actions, even when both exist in the same daemon.
func TestListActions_ProjectScoping_NeverLeaksOtherProject(t *testing.T) {
	svc, taskRepo := newActionListTestService(t)
	// card-model-cleanup PR-2: see TestListActions_NotedRoundTrips's own
	// comment — Type=Card + a non-nil Card populates the card columns at
	// create time, no separate seed step exists anymore.
	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "T1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	if err := taskRepo.CreateTask(task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	task2 := &orchestrator.Task{ProjectID: "proj-2", Title: "T2", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	if err := taskRepo.CreateTask(task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}
	if _, err := svc.ApplyAction(context.Background(), task1.ID, ApplyActionRequest{Type: "noted", Payload: []byte(`{"p":1}`)}); err != nil {
		t.Fatalf("ApplyAction task1: %v", err)
	}
	if _, err := svc.ApplyAction(context.Background(), task2.ID, ApplyActionRequest{Type: "noted", Payload: []byte(`{"p":2}`)}); err != nil {
		t.Fatalf("ApplyAction task2: %v", err)
	}

	result, err := svc.ListActions(orchestrator.ActionListFilter{ProjectIDs: []string{"proj-1"}})
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(result.Actions) != 1 || result.Actions[0].TaskID != task1.ID {
		t.Fatalf("actions = %+v, want exactly proj-1's one action", result.Actions)
	}
}

// TestListActions_CursorMonotonic_AcrossRealApplyActionCalls pins "since
// カーソルが単調に進み、同じ action を 2 度返さない" using the real write
// path (multiple ApplyAction calls, not hand-inserted rows).
func TestListActions_CursorMonotonic_AcrossRealApplyActionCalls(t *testing.T) {
	svc, taskRepo := newActionListTestService(t)
	// card-model-cleanup PR-2: see TestListActions_NotedRoundTrips's own
	// comment — no separate seed step exists anymore.
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	if err := taskRepo.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "noted", Payload: []byte(`{"i":` + strconv.Itoa(i) + `}`)}); err != nil {
			t.Fatalf("ApplyAction #%d: %v", i, err)
		}
	}

	first, err := svc.ListActions(orchestrator.ActionListFilter{ProjectIDs: []string{"proj-1"}, Limit: 2})
	if err != nil {
		t.Fatalf("ListActions page 1: %v", err)
	}
	if len(first.Actions) != 2 {
		t.Fatalf("page 1 actions = %d, want 2", len(first.Actions))
	}

	second, err := svc.ListActions(orchestrator.ActionListFilter{ProjectIDs: []string{"proj-1"}, Since: first.NextCursor})
	if err != nil {
		t.Fatalf("ListActions page 2: %v", err)
	}
	if len(second.Actions) != 1 {
		t.Fatalf("page 2 actions = %d, want 1 (the remaining action)", len(second.Actions))
	}
	if second.Actions[0].ID == first.Actions[0].ID || second.Actions[0].ID == first.Actions[1].ID {
		t.Fatalf("page 2 re-delivered an action already seen in page 1: %+v vs %+v", second.Actions, first.Actions)
	}

	third, err := svc.ListActions(orchestrator.ActionListFilter{ProjectIDs: []string{"proj-1"}, Since: second.NextCursor})
	if err != nil {
		t.Fatalf("ListActions page 3: %v", err)
	}
	if len(third.Actions) != 0 {
		t.Fatalf("page 3 actions = %d, want 0 (nothing left)", len(third.Actions))
	}
	if third.NextCursor != second.NextCursor {
		t.Errorf("empty page changed the cursor: %q -> %q", second.NextCursor, third.NextCursor)
	}
}
