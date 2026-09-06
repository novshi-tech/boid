package api

// Concurrency coverage for card_requests's single-execution-slot invariant
// between a card command launcher (RunCardCommandAsHuman) and a Go dispatch
// (acceptGo's own reservation, extracted as reserveGoCardRequest so it can
// be driven directly here).
//
// A JSON-seeded specced child cannot be used to set this race up: it is the
// exact condition cardWorkChildOccupantTx treats as "occupied" (see
// DetailOpenSlotChildID, card.go), so RunCardCommandAsHuman would always concede
// there before ever attempting the CreateCardRequest INSERT — deterministically,
// not as a race outcome. A prior version of this test seeded such a child and,
// as a result, never actually exercised the card_requests unique index: command
// lost every run for a reason unrelated to Go's own reservation logic, to the
// point that removing acceptGo's reservation call entirely left the test green.
// These tests instead drive Go's reservation directly via reserveGoCardRequest,
// with no specced child in task_triage, so both sides genuinely contend for the
// same CreateCardRequest INSERT arbitrated by idx_card_requests_active_unique.
//
// Honesty note: with a single sqlite connection (SetMaxOpenConns(1), the same
// posture production runs under), any two callers whose occupancy check and
// claim are each wrapped in one transaction can never truly interleave mid-Tx
// — so TestConcurrentCommandAndGo_OnlyOneClaimsTheSlot cannot itself reproduce
// a read-then-separately-write race; it verifies the two entry points never
// simultaneously succeed under goroutine-level concurrency, and — with cgo
// available for `-race` — that neither implementation has an unsynchronized
// shared-state bug. TestCommandThenGo/TestGoThenCommand instead pin the two
// orderings deterministically, proving each direction of arbitration on its
// own without relying on scheduling.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newCardSlotConcurrencyFixture builds a fresh in-memory DB, project, and an
// empty card (no task_triage children) plus a TaskWorkflowService wired for
// both RunCardCommandAsHuman and reserveGoCardRequest — no TaskTriage/TaskCreator,
// since none of this file's tests dispatch a full acceptGo.
func newCardSlotConcurrencyFixture(t *testing.T) (*TaskWorkflowService, *orchestrator.Task, *orchestrator.TaskRepository) {
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

	repo := orchestrator.NewTaskRepository(d.Conn)
	card := &orchestrator.Task{Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}

	jobs := newFakeTriggerJobStore()
	exec := &fakeTriggerExecDispatcher{jobs: jobs}
	svc := &TaskWorkflowService{
		Tasks:        repo,
		CardRequests: repo,
		Tx:           realTransactor{conn: d.Conn},
		Exec:         exec,
		Meta:         fakeTriggerMetaStore{byProject: map[string]*orchestrator.ProjectMeta{"proj-1": testCardMeta(map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}})}},
	}
	return svc, card, repo
}

// TestGoThenCommand_CommandSeesOccupiedViaCardRequests pins that once Go has
// reserved the slot (no specced child involved), a subsequent
// RunCardCommandAsHuman must see it occupied via the card_requests row
// itself — not via cardWorkChildOccupantTx's JSON/live-child check, which
// has nothing to see here.
func TestGoThenCommand_CommandSeesOccupiedViaCardRequests(t *testing.T) {
	svc, card, repo := newCardSlotConcurrencyFixture(t)

	goReq, err := svc.reserveGoCardRequest(card.ID)
	if err != nil || goReq == nil {
		t.Fatalf("reserveGoCardRequest: got (%+v, %v), want a claimed reservation", goReq, err)
	}

	result, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman: %v", err)
	}
	if !result.Occupied {
		t.Fatalf("RunCardCommandAsHuman: Occupied = false, want true (Go already holds the slot)")
	}

	active, err := repo.CountActiveCardRequests(card.ID)
	if err != nil {
		t.Fatalf("CountActiveCardRequests: %v", err)
	}
	if active != 1 {
		t.Fatalf("active card_requests = %d, want 1 (Go's own reservation only)", active)
	}
}

// TestCommandThenGo_GoRejectedBySlotOccupied is
// TestGoThenCommand_CommandSeesOccupiedViaCardRequests's mirror: once a
// command has claimed the slot, reserveGoCardRequest (acceptGo's own
// reservation logic) must fail with ErrCardRequestSlotOccupied.
func TestCommandThenGo_GoRejectedBySlotOccupied(t *testing.T) {
	svc, card, repo := newCardSlotConcurrencyFixture(t)

	result, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman: %v", err)
	}
	if result.Occupied {
		t.Fatalf("RunCardCommandAsHuman: Occupied = true, want false (nothing else holds the slot yet)")
	}

	goReq, err := svc.reserveGoCardRequest(card.ID)
	if !errors.Is(err, orchestrator.ErrCardRequestSlotOccupied) {
		t.Fatalf("reserveGoCardRequest: err = %v, want ErrCardRequestSlotOccupied", err)
	}
	if goReq != nil {
		t.Errorf("reserveGoCardRequest: got a reservation %+v, want nil on rejection", goReq)
	}

	active, err := repo.CountActiveCardRequests(card.ID)
	if err != nil {
		t.Fatalf("CountActiveCardRequests: %v", err)
	}
	if active != 1 {
		t.Fatalf("active card_requests = %d, want 1 (the command's own reservation only)", active)
	}
}

// TestConcurrentCommandAndGo_OnlyOneClaimsTheSlot is the goroutine-level
// smoke test: see this file's header for what it can and cannot prove.
func TestConcurrentCommandAndGo_OnlyOneClaimsTheSlot(t *testing.T) {
	svc, card, repo := newCardSlotConcurrencyFixture(t)

	var wg sync.WaitGroup
	var cmdResult *RunCardCommandResult
	var cmdErr error
	var goReq *orchestrator.CardRequest
	var goErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		cmdResult, cmdErr = svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
	}()
	go func() {
		defer wg.Done()
		goReq, goErr = svc.reserveGoCardRequest(card.ID)
	}()
	wg.Wait()

	cmdWon := cmdErr == nil && cmdResult != nil && !cmdResult.Occupied
	goWon := goErr == nil && goReq != nil
	if cmdWon == goWon {
		t.Fatalf("exactly one of {command, go} must win the race, got cmdWon=%v (result=%+v err=%v) goWon=%v (req=%+v err=%v)",
			cmdWon, cmdResult, cmdErr, goWon, goReq, goErr)
	}
	if !goWon && !errors.Is(goErr, orchestrator.ErrCardRequestSlotOccupied) {
		t.Errorf("go lost the race with err = %v, want ErrCardRequestSlotOccupied", goErr)
	}
	if !cmdWon && (cmdErr != nil || cmdResult == nil || !cmdResult.Occupied) {
		t.Errorf("command lost the race with result=%+v err=%v, want a non-error Occupied=true result", cmdResult, cmdErr)
	}

	active, err := repo.CountActiveCardRequests(card.ID)
	if err != nil {
		t.Fatalf("CountActiveCardRequests: %v", err)
	}
	if active != 1 {
		t.Fatalf("active card_requests = %d, want exactly 1 — both or neither side claimed the slot", active)
	}
}
