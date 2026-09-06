package api

// Pins RunCardCommand's manual card-command launch: claims the card's
// shared execution slot with a pre-generated launcher job id, dispatches
// the project.yaml `run:` command as a readonly exec job carrying
// card/request context, and releases the slot on a dispatch failure.
// Reuses the trigger sweep test harness since the launcher shares
// fireTrigger's StartExec shape.

import (
	"context"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newCardCommandTestService builds a TaskWorkflowService wired for
// RunCardCommand tests and creates one card task under projectID.
func newCardCommandTestService(t *testing.T, projectID string, meta *orchestrator.ProjectMeta) (*TaskWorkflowService, *fakeTriggerExecDispatcher, *orchestrator.Task) {
	t.Helper()
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{projectID: meta})
	repo := svc.Triggers.(*orchestrator.TaskRepository)
	svc.CardRequests = repo
	svc.Tasks = repo

	card := &orchestrator.Task{
		Type:      orchestrator.TaskTypeCard,
		ProjectID: projectID,
		Title:     "a card",
		Card:      &orchestrator.CardAttrs{},
	}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	return svc, exec, card
}

func testCardMeta(commands map[string]orchestrator.CardCommand) *orchestrator.ProjectMeta {
	return &orchestrator.ProjectMeta{CardCommands: commands}
}

func TestRunCardCommand_Success_ClaimsSlotAndDispatchesLauncher(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))

	result, err := svc.RunCardCommand(context.Background(), card.ID, "review", "look into this")
	if err != nil {
		t.Fatalf("RunCardCommand: %v", err)
	}
	if result.Occupied {
		t.Fatal("Occupied = true, want false for a freshly-claimed slot")
	}
	if result.RequestID == "" || result.JobID == "" {
		t.Fatalf("result = %+v, want non-empty RequestID and JobID", result)
	}

	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1", len(exec.calls))
	}
	got := exec.calls[0]
	if !got.Readonly {
		t.Error("Readonly = false, want true — a card command launcher is a readonly exec job")
	}
	if got.CardID != card.ID {
		t.Errorf("CardID = %q, want %q", got.CardID, card.ID)
	}
	if got.CardRequestID != result.RequestID {
		t.Errorf("CardRequestID = %q, want %q", got.CardRequestID, result.RequestID)
	}
	if got.JobID == "" {
		t.Error("JobID is empty, want the pre-generated launcher job id")
	}

	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	req, err := repo.GetCardRequest(result.RequestID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if req.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("status = %q, want launching", req.Status)
	}
	if req.LauncherJobID != got.JobID {
		t.Errorf("LauncherJobID = %q, want %q (must match StartExecRequest.JobID exactly, or ClaimQueuedCardRequests's precondition can never be satisfied)", req.LauncherJobID, got.JobID)
	}
	if req.CauseID != "" {
		t.Errorf("CauseID = %q, want empty — a manual command must be classified as human-origin", req.CauseID)
	}
	if req.Instruction != "look into this" {
		t.Errorf("Instruction = %q, want %q", req.Instruction, "look into this")
	}
}

func TestRunCardCommand_UnknownCommand_Returns404(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))

	_, err := svc.RunCardCommand(context.Background(), card.ID, "nonexistent", "")
	if err == nil {
		t.Fatal("want error for an undeclared command key")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("err = %v, want a 404 StatusError", err)
	}
}

func TestRunCardCommand_NotACard_Returns400(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.Triggers.(*orchestrator.TaskRepository)
	exec := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: "", Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	if err := repo.CreateTask(exec); err != nil {
		t.Fatalf("create execution task: %v", err)
	}

	_, err := svc.RunCardCommand(context.Background(), exec.ID, "review", "")
	if err == nil {
		t.Fatal("want error targeting a non-card task")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("err = %v, want a 400 StatusError", err)
	}
}

// TestRunCardCommand_Occupied_ReturnsLinkWithoutCreatingARequest pins that a
// manual command must NOT be queued behind a busy slot — it returns a link
// to the current execution, and does not create a second row.
func TestRunCardCommand_Occupied_ReturnsLinkWithoutCreatingARequest(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))

	first, err := svc.RunCardCommand(context.Background(), card.ID, "review", "first")
	if err != nil {
		t.Fatalf("first RunCardCommand: %v", err)
	}

	second, err := svc.RunCardCommand(context.Background(), card.ID, "review", "second")
	if err != nil {
		t.Fatalf("second RunCardCommand: %v", err)
	}
	if !second.Occupied {
		t.Fatal("Occupied = false, want true — the slot is still held by the first request")
	}
	if second.RequestID != first.RequestID {
		t.Errorf("second.RequestID = %q, want the first request's id %q (link to current execution, not a new row)", second.RequestID, first.RequestID)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want exactly 1 — the occupied call must not dispatch a second launcher", len(exec.calls))
	}

	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	rows, err := repo.ListCardRequestsByCard(card.ID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("card_requests rows = %d, want exactly 1 (no second row created while occupied)", len(rows))
	}
}

// TestRunCardCommand_DispatchFailure_ReleasesSlot pins that a launcher
// dispatch failure fails the claimed request (retry-able) rather than
// leaving the slot stuck forever.
func TestRunCardCommand_DispatchFailure_ReleasesSlot(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	exec.failNext = 1

	_, err := svc.RunCardCommand(context.Background(), card.ID, "review", "")
	if err == nil {
		t.Fatal("want an error when dispatch fails")
	}

	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	rows, err := repo.ListCardRequestsByCard(card.ID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("card_requests rows = %d, want exactly 1", len(rows))
	}
	if rows[0].Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("status = %q, want failed (retry-able) after a dispatch failure", rows[0].Status)
	}

	// The slot must now be free for a subsequent call to succeed.
	second, err := svc.RunCardCommand(context.Background(), card.ID, "review", "")
	if err != nil {
		t.Fatalf("RunCardCommand after release: %v", err)
	}
	if second.Occupied {
		t.Fatal("Occupied = true, want the released slot to accept a fresh claim")
	}
}
