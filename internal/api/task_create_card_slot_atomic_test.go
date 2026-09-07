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
//
// In practice the race always resolves the same way: createExecutionTask
// does behavior resolution, a project lookup, and base_branch expansion
// before it ever opens its transaction, while reserveGoCardRequest is a
// bare INSERT — Go reaches the shared card_requests unique index first on
// every observed run, so createWon below never actually triggers. That
// asymmetry also means idx_card_requests_active_unique alone does NOT
// arbitrate the reverse order (a live child already created directly,
// THEN a bare Go reservation attempt) — see
// TestReserveGoCardRequest_DoesNotSeeADirectlyCreatedLiveChild, which pins
// that directly and documents where the real protection for that order
// lives instead (acceptGo's own pre-checks, not this unique index).

import (
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// countingRealTransactor wraps another Transactor and counts WithinTx
// calls, so a test can pin "exactly one transaction" rather than just
// "the end state looks right" (which a two-transaction sequence could
// produce just as well).
type countingRealTransactor struct {
	inner Transactor
	calls int
}

func (t *countingRealTransactor) WithinTx(fn func(TxStore) error) error {
	t.calls++
	return t.inner.WithinTx(fn)
}

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

// TestReserveGoCardRequest_DoesNotSeeADirectlyCreatedLiveChild pins the
// asymmetry this file's header describes: idx_card_requests_active_unique
// only indexes card_requests, so a bare reserveGoCardRequest call has
// nothing to collide with when a direct `--parent <card>` create already
// made the card's live child moments earlier — it succeeds. acceptGo's
// real production flow never actually hits this: its own unresolvedCount
// pre-check (ListChildren, before reserveGoCardRequest is ever called)
// rejects first, and if a reservation somehow still got made, the
// non-atomic cardSlotConflictWithRequests check in createExecutionTask's
// child-task-ify step would 409 and releaseReservation would unwind it.
// reserveGoCardRequest alone is not that whole flow, so this test's
// success here documents where the real protection lives instead of
// implying reserveGoCardRequest itself provides it.
func TestReserveGoCardRequest_DoesNotSeeADirectlyCreatedLiveChild(t *testing.T) {
	taskSvc, goSvc, card, repo := newAtomicCardSlotFixture(t)

	if _, err := taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1", Title: "a new child", Behavior: "dev", ParentID: card.ID, Ref: "ch_00",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if _, err := goSvc.reserveGoCardRequest(card.ID); err != nil {
		t.Fatalf("reserveGoCardRequest: %v, want success — a live child with no card_requests row of its own does not block a bare reservation", err)
	}

	active, err := repo.CountActiveCardRequests(card.ID)
	if err != nil {
		t.Fatalf("CountActiveCardRequests: %v", err)
	}
	if active != 1 {
		t.Fatalf("active card_requests = %d, want 1 (the reservation succeeded alongside the live child)", active)
	}
}

// ---- JSON-child conflict cases, ported onto the atomic (Tx-wired) fixture.
//
// task_create_card_slot_test.go's stub-based tests exercise the SAME shared
// cardChildSlotConflict/cardSlotConflictWithLister logic, but through the
// non-atomic fallback (no Tx wired) — since wire.go now always wires Tx in
// production, createExecutionTask's real, shipped behavior for a card
// parent takes the atomicCardCheck branch instead. These three mirror the
// JSON open-child reject, the specced child's own-reservation exception,
// and the ref/project/behavior spoofing guard through that actual branch.

func TestCreateTask_AtomicPath_RejectsWhenCardSlotOccupiedByOpenJSONChild(t *testing.T) {
	taskSvc, _, card, repo := newAtomicCardSlotFixture(t)
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{
		TaskID: card.ID,
		Detail: []byte(`{"children":[{"id":"ch_00","status":"open"}]}`),
	}); err != nil {
		t.Fatalf("seed task_triage: %v", err)
	}

	_, err := taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1", Title: "a new child", Behavior: "dev", ParentID: card.ID, Ref: "some-other-id",
	})
	if err == nil {
		t.Fatal("expected rejection creating a second child while an open JSON child occupies the slot")
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

func TestCreateTask_AtomicPath_AllowsFulfillingTheSpeccedChildsOwnReservation(t *testing.T) {
	taskSvc, _, card, repo := newAtomicCardSlotFixture(t)
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{
		TaskID: card.ID,
		Detail: []byte(`{"children":[{"id":"ch_00","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}]}`),
	}); err != nil {
		t.Fatalf("seed task_triage: %v", err)
	}

	got, err := taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1", Title: "do it", Behavior: "dev", ParentID: card.ID, Ref: "ch_00",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (fulfills ch_00's own reservation)", err)
	}
	if got == nil {
		t.Fatal("expected a task to have been created")
	}
}

func TestCreateTask_AtomicPath_RejectsRefMatchWithMismatchedProjectOrBehavior(t *testing.T) {
	taskSvc, _, card, repo := newAtomicCardSlotFixture(t)
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{
		TaskID: card.ID,
		Detail: []byte(`{"children":[{"id":"ch_00","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}]}`),
	}); err != nil {
		t.Fatalf("seed task_triage: %v", err)
	}

	_, err := taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-attacker", Title: "planted", Behavior: "dev", ParentID: card.ID, Ref: "ch_00",
	})
	if err == nil {
		t.Fatal("expected rejection: ref matches the occupant but project does not match its own spec")
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

// TestCreateTask_AtomicPath_CardRequestIDCarrying_RoutesThroughOneTx pins
// that a CardRequestID-carrying create under a card (acceptGo's own child
// dispatch — the only caller that ever passes both ParentID=<the card> and
// a non-empty CardRequestID to CreateTask; a card-command launcher's own
// continuation is instead forced to a ROOT ParentID, internal/server/boid_executor.go)
// now takes the SAME atomic WithinTx branch as a CardRequestID-less direct
// create, rather than a separate non-transactional pre-check followed by
// its own transaction: the card_requests row ends up "attached" to the new
// task, and the slot re-check inside that one transaction correctly
// excludes the caller's own reservation.
func TestCreateTask_AtomicPath_CardRequestIDCarrying_RoutesThroughOneTx(t *testing.T) {
	taskSvc, goSvc, card, repo := newAtomicCardSlotFixture(t)

	// Count WithinTx calls: since CardRequestLinker is unset on this fixture,
	// a regression back to the old two-round-trip path (a non-transactional
	// pre-check, THEN a separate CreateTaskLinkedToCardRequest call opening
	// its OWN transaction) would still succeed functionally — only the call
	// count below actually distinguishes "one atomic WithinTx" from that.
	counting := &countingRealTransactor{inner: taskSvc.Tx}
	taskSvc.Tx = counting

	cardReq, err := goSvc.reserveGoCardRequest(card.ID)
	if err != nil {
		t.Fatalf("reserveGoCardRequest: %v", err)
	}

	got, err := taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1", Title: "do it", Behavior: "dev",
		ParentID: card.ID, Ref: "ch_00", CardRequestID: cardReq.ID, CardRequestOwnerJobID: cardReq.LauncherJobID,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (fulfills its own reservation)", err)
	}
	if counting.calls != 1 {
		t.Fatalf("WithinTx calls = %d, want exactly 1 — the re-check and the linked create must share one transaction", counting.calls)
	}

	updated, gerr := repo.GetCardRequest(cardReq.ID)
	if gerr != nil {
		t.Fatalf("GetCardRequest: %v", gerr)
	}
	if updated.Status != orchestrator.CardRequestStatusAttached || updated.TargetID != got.ID {
		t.Errorf("card request = %+v, want attached to the new task %q", updated, got.ID)
	}

	children, lerr := repo.ListChildren(card.ID)
	if lerr != nil {
		t.Fatalf("ListChildren: %v", lerr)
	}
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}
}

// TestCreateTask_AtomicPath_CardRequestIDCarrying_RejectsWhenAnotherOccupantExists
// pins the slot re-check side of the same branch: a live child that isn't
// the reservation's own (i.e. NOT excluded by ownCardRequestID) still
// blocks, even for a CardRequestID-carrying create.
func TestCreateTask_AtomicPath_CardRequestIDCarrying_RejectsWhenAnotherOccupantExists(t *testing.T) {
	taskSvc, goSvc, card, repo := newAtomicCardSlotFixture(t)

	if _, err := taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1", Title: "already there", Behavior: "dev", ParentID: card.ID, Ref: "ch_other",
	}); err != nil {
		t.Fatalf("seed occupant create: %v", err)
	}

	cardReq, err := goSvc.reserveGoCardRequest(card.ID)
	if err != nil {
		t.Fatalf("reserveGoCardRequest: %v (documented to succeed even with a live child already present)", err)
	}

	_, err = taskSvc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1", Title: "do it", Behavior: "dev",
		ParentID: card.ID, Ref: "ch_00", CardRequestID: cardReq.ID, CardRequestOwnerJobID: cardReq.LauncherJobID,
	})
	if err == nil {
		t.Fatal("expected rejection: another live child already occupies the slot")
	}
	se2, ok2 := err.(*StatusError)
	if !ok2 || se2.Code != 409 {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}

	children, lerr := repo.ListChildren(card.ID)
	if lerr != nil {
		t.Fatalf("ListChildren: %v", lerr)
	}
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1 (only the pre-existing occupant) — the rejected create must not have inserted a second", len(children))
	}
}
