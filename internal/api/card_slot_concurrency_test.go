package api

// Concurrent smoke test: a card command launcher (RunCardCommand) and a Go
// dispatch (acceptGo) must never both succeed in claiming the same card's
// single execution slot.
//
// Honesty note: with a single sqlite connection (SetMaxOpenConns(1), the
// same posture production runs under), any two callers whose occupancy
// check and claim are each wrapped in one transaction can never truly
// interleave — so this test cannot itself reproduce a read-then-separately-
// write race. What it verifies, on every run: the two entry points never
// simultaneously succeed under goroutine-level concurrency, and — with cgo
// available for `-race` — that neither implementation has an unsynchronized
// shared-state bug. The occupancy-check-and-claim atomicity itself
// (cardWorkChildOccupantTx and the CreateCardRequest INSERT sharing one
// WithinTx call, in both RunCardCommand and acceptGo) is a structural
// property to verify by code review, not by racing goroutines against a
// single-connection database.

import (
	"context"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestConcurrentCommandAndGo_OnlyOneClaimsTheSlot(t *testing.T) {
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
	// A specced child ready for Go to dispatch.
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{
		TaskID: card.ID,
		Detail: []byte(`{"children":[{"id":"ch_00","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}]}`),
	}); err != nil {
		t.Fatalf("seed task_triage: %v", err)
	}

	jobs := newFakeTriggerJobStore()
	exec := &fakeTriggerExecDispatcher{jobs: jobs}
	// creator mimics createExecutionTask's own CardRequestLinker branch
	// (real production code, api/task_create.go) closely enough for this
	// race: attach the reservation to the freshly-created task id, same
	// atomic-from-the-DB's-perspective operation (a single AttachCardRequest
	// UPDATE), without pulling in the full auto_start/dispatch pipeline this
	// test has no need to exercise.
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
		task := &orchestrator.Task{
			ProjectID: req.ProjectID, Type: orchestrator.TaskTypeExecution, ParentID: req.ParentID,
			Ref: req.Ref, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: req.Behavior},
		}
		if err := repo.CreateTask(task); err != nil {
			return nil, err
		}
		if req.CardRequestID != "" {
			if err := repo.AttachCardRequest(req.CardRequestID, orchestrator.CardRequestTargetKindTask, task.ID); err != nil {
				return nil, err
			}
		}
		return task, nil
	}}

	svc := &TaskWorkflowService{
		Tasks:        repo,
		TaskTriage:   repo,
		CardRequests: repo,
		Tx:           realTransactor{conn: d.Conn},
		Exec:         exec,
		Meta:         fakeTriggerMetaStore{byProject: map[string]*orchestrator.ProjectMeta{"proj-1": testCardMeta(map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}})}},
		TaskCreator:  creator,
	}

	var wg sync.WaitGroup
	var cmdResult *RunCardCommandResult
	var cmdErr error
	var goErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		cmdResult, cmdErr = svc.RunCardCommand(context.Background(), card.ID, "review", "")
	}()
	go func() {
		defer wg.Done()
		_, goErr = svc.acceptGo(context.Background(), card.ID, false)
	}()
	wg.Wait()

	cmdWon := cmdErr == nil && cmdResult != nil && !cmdResult.Occupied
	goWon := goErr == nil
	if cmdWon == goWon {
		t.Fatalf("exactly one of {command, go} must win the race, got cmdWon=%v (result=%+v err=%v) goWon=%v (err=%v)",
			cmdWon, cmdResult, cmdErr, goWon, goErr)
	}

	// After both goroutines finish, the card's slot must have exactly one
	// active (launching/attached) card_requests row, or none if the loser's
	// own reservation attempt never got as far as a successful INSERT —
	// never two, and the live-child count must agree with which side won.
	active, err := repo.CountActiveCardRequests(card.ID)
	if err != nil {
		t.Fatalf("CountActiveCardRequests: %v", err)
	}
	if active > 1 {
		t.Fatalf("active card_requests = %d, want at most 1 — both sides claimed the slot", active)
	}

	fresh, err := repo.GetTask(card.ID)
	if err != nil {
		t.Fatalf("GetTask(card): %v", err)
	}
	if goWon {
		if fresh.OpenChildCount != 1 {
			t.Errorf("OpenChildCount = %d, want 1 (Go's own dispatched child) since Go won the race", fresh.OpenChildCount)
		}
		if len(exec.calls) != 0 {
			t.Errorf("StartExec calls = %d, want 0 — the command must not have dispatched alongside Go's win", len(exec.calls))
		}
	} else {
		if fresh.OpenChildCount != 0 {
			t.Errorf("OpenChildCount = %d, want 0 (Go must not have dispatched a child) since the command won the race", fresh.OpenChildCount)
		}
		if len(exec.calls) != 1 {
			t.Errorf("StartExec calls = %d, want 1 — the winning command must have dispatched its launcher", len(exec.calls))
		}
	}
}
