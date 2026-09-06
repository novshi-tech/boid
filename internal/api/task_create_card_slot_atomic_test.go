package api

// Coverage for createExecutionTask's atomic path (task_create.go): a direct
// `--parent <card>` execution-task create takes no card_requests row of its
// own, so it cannot rely on idx_card_requests_active_unique to arbitrate
// against a concurrent RunCardCommandAsHuman/acceptGo the way those two arbitrate
// each other (card_slot_concurrency_test.go). When s.Tx is wired, the
// slot re-check and the INSERT run inside one WithinTx call instead — these
// tests exercise that path against a real DB, not the plain-struct stubs
// task_create_card_slot_test.go uses for the (still-supported, Tx-unwired)
// non-atomic fallback.
//
// Honesty note: unlike card_slot_concurrency_test.go's pairing (where both
// sides are each a single atomic call, so a goroutine race meaningfully
// pins "exactly one wins"), the TOCTOU this file's atomic path closes is a
// gap BETWEEN two separate calls (the pre-check, then later the INSERT) in
// the fallback it replaces. Reproducing that gap under SetMaxOpenConns(1)
// would need the scheduler to preempt the racing goroutine exactly between
// those two calls — confirmed NOT to happen reliably (300 reps against a
// deliberately-reintroduced version of the gap never once observed a
// double-booking). So TestCreateTask_AtomicPath_RaceWithGoReservation below
// is a smoke/regression test only (still useful for -race's shared-state
// coverage and for pinning the fixed code's observable outcome), not proof
// the gap is closed — that guarantee is structural: cardSlotConflictWithLister
// and tx.CreateTask sharing the one WithinTx call in createExecutionTask is
// a property to verify by reading that code, the same posture
// card_slot_concurrency_test.go's own header takes for
// cardWorkChildOccupantTx/CreateCardRequest's atomicity.

import (
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newAtomicCardSlotFixture(t *testing.T) (taskSvc *TaskAppService, goSvc *TaskWorkflowService, card *orchestrator.Task, repo *orchestrator.TaskRepository) {
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
		t.Fatalf("create project: %v", err)
	}

	repo = orchestrator.NewTaskRepository(d.Conn)
	card = &orchestrator.Task{Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}

	taskSvc = &TaskAppService{
		Tasks:        repo,
		Meta:         stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		CardRequests: repo,
		Tx:           realTransactor{conn: d.Conn},
	}
	goSvc = &TaskWorkflowService{
		Tasks:        repo,
		CardRequests: repo,
		Tx:           realTransactor{conn: d.Conn},
	}
	return taskSvc, goSvc, card, repo
}

// TestCreateTask_AtomicPath_RejectsWhenGoReservationAlreadyActive pins that
// a Go reservation (no live/JSON child at all, only a card_requests row)
// already claiming the slot must block a direct `--parent <card>` create
// too, not just RunCardCommandAsHuman/acceptGo.
func TestCreateTask_AtomicPath_RejectsWhenGoReservationAlreadyActive(t *testing.T) {
	taskSvc, goSvc, card, repo := newAtomicCardSlotFixture(t)

	if _, err := goSvc.reserveGoCardRequest(card.ID); err != nil {
		t.Fatalf("reserveGoCardRequest: %v", err)
	}

	_, err := taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "a new child",
		Behavior:  "dev",
		ParentID:  card.ID,
		Ref:       "ch_00",
	})
	if err == nil {
		t.Fatal("expected rejection: Go already holds the card's slot")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != 409 {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}

	children, lerr := repo.ListChildren(card.ID)
	if lerr != nil {
		t.Fatalf("ListChildren: %v", lerr)
	}
	if len(children) != 0 {
		t.Fatalf("children = %d, want 0 — the rejected create must not have inserted a task", len(children))
	}
}

// TestCreateTask_AtomicPath_SucceedsWhenSlotFree is the sanity companion:
// with nothing occupying the slot, the atomic path must still create the
// task normally end-to-end against a real DB.
func TestCreateTask_AtomicPath_SucceedsWhenSlotFree(t *testing.T) {
	taskSvc, _, card, repo := newAtomicCardSlotFixture(t)

	got, err := taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "a new child",
		Behavior:  "dev",
		ParentID:  card.ID,
		Ref:       "ch_00",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (empty slot)", err)
	}
	if got == nil {
		t.Fatal("expected a task to have been created")
	}
	children, lerr := repo.ListChildren(card.ID)
	if lerr != nil {
		t.Fatalf("ListChildren: %v", lerr)
	}
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}
}

// TestCreateTask_AtomicPath_RaceWithGoReservation is the goroutine-level
// smoke test, same honesty-note caveat as
// TestConcurrentCommandAndGo_OnlyOneClaimsTheSlot (card_slot_concurrency_test.go):
// SetMaxOpenConns(1) means the two WithinTx bodies below can never truly
// interleave, so this cannot reproduce a read-then-separately-write race —
// it verifies the two entry points never simultaneously succeed, and that
// exactly one live-child/card_requests occupant results either way.
func TestCreateTask_AtomicPath_RaceWithGoReservation(t *testing.T) {
	taskSvc, goSvc, card, repo := newAtomicCardSlotFixture(t)

	type createResult struct {
		task *orchestrator.Task
		err  error
	}
	var wg sync.WaitGroup
	var createRes createResult
	var goReq *orchestrator.CardRequest
	var goErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		createRes.task, createRes.err = taskSvc.CreateTask(CreateTaskRequest{
			ProjectID: "proj-1", Title: "a new child", Behavior: "dev", ParentID: card.ID, Ref: "ch_00",
		})
	}()
	go func() {
		defer wg.Done()
		goReq, goErr = goSvc.reserveGoCardRequest(card.ID)
	}()
	wg.Wait()

	createWon := createRes.err == nil && createRes.task != nil
	goWon := goErr == nil && goReq != nil
	if createWon == goWon {
		t.Fatalf("exactly one of {direct create, go} must win, got createWon=%v (err=%v) goWon=%v (err=%v)",
			createWon, createRes.err, goWon, goErr)
	}

	children, lerr := repo.ListChildren(card.ID)
	if lerr != nil {
		t.Fatalf("ListChildren: %v", lerr)
	}
	active, aerr := repo.CountActiveCardRequests(card.ID)
	if aerr != nil {
		t.Fatalf("CountActiveCardRequests: %v", aerr)
	}
	if createWon {
		if len(children) != 1 {
			t.Errorf("children = %d, want 1 (the direct create's own child) since it won", len(children))
		}
		if active != 0 {
			t.Errorf("active card_requests = %d, want 0 since Go lost", active)
		}
	} else {
		if len(children) != 0 {
			t.Errorf("children = %d, want 0 since the direct create lost", len(children))
		}
		if active != 1 {
			t.Errorf("active card_requests = %d, want 1 (Go's own reservation) since it won", active)
		}
	}
}
