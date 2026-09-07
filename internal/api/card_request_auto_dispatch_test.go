package api

// Pins the automatic dispatcher (card_request_auto_dispatch.go): the
// queued->launching claim through orchestrator.ClaimQueuedCardRequestsForDispatch,
// the launcher exec dispatch, and the five commit-triggered call sites that
// attempt an immediate dispatch right after their own transaction commits,
// never waiting on the periodic sweep.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func enqueueForDispatch(t *testing.T, svc *TaskWorkflowService, cardID, commandKey, causeID string) *orchestrator.CardRequest {
	t.Helper()
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: commandKey, CauseID: causeID}
	if err := svc.CardRequests.CreateCardRequest(req); err != nil {
		t.Fatalf("enqueue card request: %v", err)
	}
	return req
}

func TestDispatchQueuedCardRequest_Success(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	req := enqueueForDispatch(t, svc, card.ID, "review", "cause-1")

	dispatched, err := svc.dispatchQueuedCardRequest(context.Background(), card.ID)
	if err != nil {
		t.Fatalf("dispatchQueuedCardRequest: %v", err)
	}
	if !dispatched {
		t.Fatal("dispatched = false, want true")
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1", len(exec.calls))
	}
	got := exec.calls[0]
	if !got.Readonly {
		t.Error("Readonly = false, want true")
	}
	if got.CardID != card.ID {
		t.Errorf("CardID = %q, want %q", got.CardID, card.ID)
	}

	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	persisted, err := repo.GetCardRequest(req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if persisted.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("Status = %q, want launching", persisted.Status)
	}
	if got.CardRequestID != persisted.ID {
		t.Errorf("StartExecRequest.CardRequestID = %q, want %q", got.CardRequestID, persisted.ID)
	}
}

func TestDispatchQueuedCardRequest_NoQueued_NoOp(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))

	dispatched, err := svc.dispatchQueuedCardRequest(context.Background(), card.ID)
	if err != nil {
		t.Fatalf("dispatchQueuedCardRequest: %v", err)
	}
	if dispatched {
		t.Error("dispatched = true, want false with nothing queued")
	}
	if len(exec.calls) != 0 {
		t.Errorf("StartExec calls = %d, want 0", len(exec.calls))
	}
}

// TestDispatchQueuedCardRequest_UndeclaredCommand_FailsRowWithoutDispatch
// pins that a queued request whose command_key no longer resolves to a
// project.yaml card_commands entry is failed explicitly, not launched and
// not left queued forever.
func TestDispatchQueuedCardRequest_UndeclaredCommand_FailsRowWithoutDispatch(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	req := enqueueForDispatch(t, svc, card.ID, "vanished", "cause-1")

	dispatched, err := svc.dispatchQueuedCardRequest(context.Background(), card.ID)
	if err != nil {
		t.Fatalf("dispatchQueuedCardRequest: %v", err)
	}
	if dispatched {
		t.Error("dispatched = true, want false for an undeclared command")
	}
	if len(exec.calls) != 0 {
		t.Errorf("StartExec calls = %d, want 0", len(exec.calls))
	}
	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	persisted, err := repo.GetCardRequest(req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if persisted.Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("Status = %q, want failed (explicit failure, not silent abandonment)", persisted.Status)
	}
}

// TestDispatchQueuedCardRequest_StartExecFailure_FailsClaimAndRequeuesFold
// pins that a StartExec failure fails the CLAIMED (launching) row but
// returns its folded sibling to queued rather than losing it.
func TestDispatchQueuedCardRequest_StartExecFailure_FailsClaimAndRequeuesFold(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	first := enqueueForDispatch(t, svc, card.ID, "review", "cause-1")
	second := enqueueForDispatch(t, svc, card.ID, "review", "cause-2")
	exec.failNext = 1

	dispatched, err := svc.dispatchQueuedCardRequest(context.Background(), card.ID)
	if err == nil {
		t.Fatal("dispatchQueuedCardRequest: want an error when StartExec fails")
	}
	if dispatched {
		t.Error("dispatched = true, want false on a StartExec failure")
	}

	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	gotFirst, err := repo.GetCardRequest(first.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(first): %v", err)
	}
	if gotFirst.Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("first.Status = %q, want failed", gotFirst.Status)
	}
	gotSecond, err := repo.GetCardRequest(second.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(second): %v", err)
	}
	if gotSecond.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("second.Status = %q, want queued (fold released back, not lost)", gotSecond.Status)
	}
}

func TestSweepQueuedCardRequests_DispatchesEveryEligibleCard(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	enqueueForDispatch(t, svc, card.ID, "review", "cause-1")

	dispatched, err := svc.SweepQueuedCardRequests(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepQueuedCardRequests: %v", err)
	}
	if len(dispatched) != 1 || dispatched[0] != card.ID {
		t.Fatalf("dispatched = %v, want [%q]", dispatched, card.ID)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1", len(exec.calls))
	}
}

// TestDispatchQueuedCardRequest_ConcurrentCallersClaimOnlyOnce races several
// callers (simulating a commit-triggered attempt and the periodic sweep
// landing at nearly the same moment) against ONE queued request on the same
// card. Exactly one may claim and dispatch it; every other caller must see
// "nothing queued" — idx_card_requests_active_unique is what makes this true
// even across the claim-then-StartExec window this test also exercises via
// exec.delay. Run with -race.
func TestDispatchQueuedCardRequest_ConcurrentCallersClaimOnlyOnce(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	enqueueForDispatch(t, svc, card.ID, "review", "cause-1")
	exec.delay = 20 * time.Millisecond // wide enough for a concurrent caller's own DB round-trip to land inside it.

	const callers = 5
	var wg sync.WaitGroup
	dispatchedCount := make([]bool, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := svc.dispatchQueuedCardRequest(context.Background(), card.ID)
			dispatchedCount[i], errs[i] = ok, err
		}(i)
	}
	wg.Wait()

	won := 0
	for i, d := range dispatchedCount {
		if errs[i] != nil {
			t.Errorf("caller %d: unexpected error: %v", i, errs[i])
		}
		if d {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("callers reporting dispatched=true = %d, want exactly 1", won)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want exactly 1 (no double dispatch)", len(exec.calls))
	}
}

// ---- commit-triggered immediate dispatch: the five hook sites ----

// TestApplyAction_CommitTriggersImmediateDispatch pins the primary (non-
// periodic) dispatch path for the generic ApplyAction funnel (noted/
// attrs_set): a queued row already sitting on the card is dispatched right
// after the action's own commit, without any periodic sweep involved.
func TestApplyAction_CommitTriggersImmediateDispatch(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	enqueueForDispatch(t, svc, card.ID, "review", "cause-1")

	if _, err := svc.ApplyAction(context.Background(), card.ID, ApplyActionRequest{Type: "noted", Payload: []byte(`{"note":"hi"}`)}); err != nil {
		t.Fatalf("ApplyAction(noted): %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1 (immediate dispatch after commit)", len(exec.calls))
	}
}

// TestApplyAnswered_CommitTriggersImmediateDispatch covers the "answered"
// action, which bypasses the generic ApplyAction pipeline entirely
// (applyAnswered, suggestion_accept.go) and needs its own hook.
func TestApplyAnswered_CommitTriggersImmediateDispatch(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	enqueueForDispatch(t, svc, card.ID, "review", "cause-1")

	if _, err := svc.ApplyAction(context.Background(), card.ID, ApplyActionRequest{Type: "answered", Payload: []byte(`{"answer":"reject"}`)}); err != nil {
		t.Fatalf("ApplyAction(answered/reject): %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1 (immediate dispatch after commit)", len(exec.calls))
	}
}

// TestRecordChildClosedOnParent_CommitTriggersImmediateDispatch covers the
// child_closed self-record path (finalizeTerminal's funnel).
func TestRecordChildClosedOnParent_CommitTriggersImmediateDispatch(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	enqueueForDispatch(t, svc, card.ID, "review", "cause-1")

	repo := svc.Tasks.(*orchestrator.TaskRepository)
	child := &orchestrator.Task{ProjectID: card.ProjectID, ParentID: card.ID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{}}
	if err := repo.CreateTask(child); err != nil {
		t.Fatalf("create child task: %v", err)
	}
	detail, err := orchestrator.SetDetailChildren(nil, []orchestrator.TaskTriageChild{
		{ID: "child-1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: child.ID},
	})
	if err != nil {
		t.Fatalf("SetDetailChildren: %v", err)
	}
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{TaskID: card.ID, Detail: detail}); err != nil {
		t.Fatalf("UpsertTaskTriage: %v", err)
	}

	svc.recordChildClosedOnParent(context.Background(), child)

	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1 (immediate dispatch after child_closed commit)", len(exec.calls))
	}
}

// TestRecordWakeDue_CommitTriggersImmediateDispatch covers the wake_due
// self-record path (queue_sweep.go's SweepWake).
func TestRecordWakeDue_CommitTriggersImmediateDispatch(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	enqueueForDispatch(t, svc, card.ID, "review", "cause-1")

	repo := svc.Tasks.(*orchestrator.TaskRepository)
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{TaskID: card.ID}); err != nil {
		t.Fatalf("UpsertTaskTriage: %v", err)
	}

	if err := svc.recordWakeDue(context.Background(), card.ID); err != nil {
		t.Fatalf("recordWakeDue: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1 (immediate dispatch after wake_due commit)", len(exec.calls))
	}
}

// TestRecordVanishedChildClosedOnParent_CommitTriggersImmediateDispatch
// covers the vanished-child child_closed self-record path
// (queue_sweep.go's SweepReconcileChildren).
func TestRecordVanishedChildClosedOnParent_CommitTriggersImmediateDispatch(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	enqueueForDispatch(t, svc, card.ID, "review", "cause-1")

	repo := svc.Tasks.(*orchestrator.TaskRepository)
	detail, err := orchestrator.SetDetailChildren(nil, []orchestrator.TaskTriageChild{
		{ID: "child-1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "vanished-task-id"},
	})
	if err != nil {
		t.Fatalf("SetDetailChildren: %v", err)
	}
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{TaskID: card.ID, Detail: detail}); err != nil {
		t.Fatalf("UpsertTaskTriage: %v", err)
	}

	svc.recordVanishedChildClosedOnParent(context.Background(), card.ID, "vanished-task-id")

	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1 (immediate dispatch after commit)", len(exec.calls))
	}
}

// TestReleaseCardRequestForTerminalTask_CommitTriggersImmediateDispatch
// covers the OTHER commit-triggered dispatch point: a freed execution slot,
// not a freshly-queued row. A task-kind continuation reaching done releases
// its card_requests row, and — with another request already queued behind
// it — the freed slot is claimed immediately.
func TestReleaseCardRequestForTerminalTask_CommitTriggersImmediateDispatch(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.Tasks.(*orchestrator.TaskRepository)
	cardRequests := svc.CardRequests.(*orchestrator.TaskRepository)

	continuation := &orchestrator.Task{ProjectID: card.ProjectID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	if err := repo.CreateTask(continuation); err != nil {
		t.Fatalf("create continuation task: %v", err)
	}
	occupying := &orchestrator.CardRequest{CardID: card.ID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-occupying"}
	if err := cardRequests.CreateCardRequest(occupying); err != nil {
		t.Fatalf("create occupying request: %v", err)
	}
	if err := cardRequests.AttachCardRequestOwned(occupying.ID, "launcher-occupying", orchestrator.CardRequestTargetKindTask, continuation.ID); err != nil {
		t.Fatalf("attach occupying request: %v", err)
	}
	// Queued behind the occupant — only claimable once its slot frees.
	enqueueForDispatch(t, svc, card.ID, "review", "cause-1")

	continuation.Status = orchestrator.TaskStatusDone
	svc.releaseCardRequestForTerminalTask(context.Background(), continuation)

	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1 (immediate dispatch after the slot freed)", len(exec.calls))
	}

	released, err := cardRequests.GetCardRequest(occupying.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(occupying): %v", err)
	}
	if released.Status != orchestrator.CardRequestStatusFinished {
		t.Errorf("occupying.Status = %q, want finished", released.Status)
	}
}
