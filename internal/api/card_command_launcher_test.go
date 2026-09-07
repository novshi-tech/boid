package api

// Pins RunCardCommandAsHuman's manual card-command launch: claims the card's
// shared execution slot with a pre-generated launcher job id, dispatches
// the project.yaml `run:` command as a readonly exec job carrying
// card/request context, and releases the slot on a dispatch failure.
// Reuses the trigger sweep test harness since the launcher shares
// fireTrigger's StartExec shape.

import (
	"context"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newCardCommandTestService builds a TaskWorkflowService wired for
// RunCardCommandAsHuman tests and creates one card task under projectID.
//
// Builds its own DB/repo (rather than delegating to newTriggerSweepTestService)
// so it can ALSO wire Tx — RunCardCommandAsHuman's occupancy check and its
// CreateCardRequest claim run inside one transaction, which
// newTriggerSweepTestService's callers never needed since fireTrigger has
// no such requirement.
func newCardCommandTestService(t *testing.T, projectID string, meta *orchestrator.ProjectMeta) (*TaskWorkflowService, *fakeTriggerExecDispatcher, *orchestrator.Task) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: projectID, WorkDir: "/tmp/" + projectID}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	repo := orchestrator.NewTaskRepository(d.Conn)
	jobs := newFakeTriggerJobStore()
	exec := &fakeTriggerExecDispatcher{jobs: jobs}

	svc := &TaskWorkflowService{
		Triggers:     repo,
		Projects:     orchestrator.NewProjectRepository(d.Conn),
		Meta:         fakeTriggerMetaStore{byProject: map[string]*orchestrator.ProjectMeta{projectID: meta}},
		Jobs:         jobs,
		Exec:         exec,
		CardRequests: repo,
		Tasks:        repo,
		TaskTriage:   repo,
		Tx:           realTransactor{conn: d.Conn},
	}

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

func TestRunCardCommandAsHuman_Success_ClaimsSlotAndDispatchesLauncher(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))

	result, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "look into this")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman: %v", err)
	}
	if result.Occupied {
		t.Fatal("Occupied = true, want false for a freshly-claimed slot")
	}
	if result.RequestID == "" || result.LauncherJobID == "" {
		t.Fatalf("result = %+v, want non-empty RequestID and LauncherJobID", result)
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

// TestRunCardCommandAsHuman_CardWrite_ThreadsIntoLaunchedDefinition pins that
// a project.yaml card_commands entry's card_write: true reaches the created
// card_requests row's Launched snapshot — the value `boid card context`
// later reports to the continuation. A command that omits card_write must
// snapshot false (opt-in, not opt-out).
func TestRunCardCommandAsHuman_CardWrite_ThreadsIntoLaunchedDefinition(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"discuss": {Label: "Discuss", Run: "echo hi", CardWrite: true},
		"review":  {Label: "Run", Run: "echo hi"},
	}))

	discussResult, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "discuss", "")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman(discuss): %v", err)
	}
	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	discussReq, err := repo.GetCardRequest(discussResult.RequestID)
	if err != nil {
		t.Fatalf("GetCardRequest(discuss): %v", err)
	}
	if !discussReq.Launched.CardWrite {
		t.Errorf("discuss request Launched.CardWrite = false, want true (card_write: true in project.yaml)")
	}

	// Release the slot before launching the second command.
	if err := repo.FailCardRequest(discussResult.RequestID, "test cleanup"); err != nil {
		t.Fatalf("FailCardRequest: %v", err)
	}

	reviewResult, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman(review): %v", err)
	}
	reviewReq, err := repo.GetCardRequest(reviewResult.RequestID)
	if err != nil {
		t.Fatalf("GetCardRequest(review): %v", err)
	}
	if reviewReq.Launched.CardWrite {
		t.Errorf("review request Launched.CardWrite = true, want false (card_write omitted in project.yaml)")
	}
}

// TestRunCardCommandAsHuman_WorkingCard_Succeeds pins the OTHER side of the
// parked/working guard: a card already `working` (not just freshly `parked`)
// must still let a card command launch. Every other test in this file that
// exercises the happy path uses a freshly-created card, which defaults to
// `parked` — nothing here previously pinned that `working` passes too, so
// tightening either guard to `!= parked` (dropping the `working` half)
// would leave this whole suite green.
func TestRunCardCommandAsHuman_WorkingCard_Succeeds(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.Tasks.(*orchestrator.TaskRepository)
	card.Status = orchestrator.TaskStatusWorking
	if err := repo.UpdateTask(card); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	result, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "look into this")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman: %v, want success for a working card", err)
	}
	if result.Occupied {
		t.Fatal("Occupied = true, want false for a freshly-claimed slot")
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1", len(exec.calls))
	}
}

func TestRunCardCommandAsHuman_UnknownCommand_Returns404(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))

	_, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "nonexistent", "")
	if err == nil {
		t.Fatal("want error for an undeclared command key")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("err = %v, want a 404 StatusError", err)
	}
}

func TestRunCardCommandAsHuman_NotACard_Returns400(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.Triggers.(*orchestrator.TaskRepository)
	exec := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: "", Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	if err := repo.CreateTask(exec); err != nil {
		t.Fatalf("create execution task: %v", err)
	}

	_, err := svc.RunCardCommandAsHuman(context.Background(), exec.ID, "review", "")
	if err == nil {
		t.Fatal("want error targeting a non-card task")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("err = %v, want a 400 StatusError", err)
	}
}

// TestRunCardCommandAsHuman_Occupied_ReturnsLinkWithoutCreatingARequest pins that a
// manual command must NOT be queued behind a busy slot — it returns a link
// to the current execution, and does not create a second row.
func TestRunCardCommandAsHuman_Occupied_ReturnsLinkWithoutCreatingARequest(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))

	first, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "first")
	if err != nil {
		t.Fatalf("first RunCardCommandAsHuman: %v", err)
	}

	second, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "second")
	if err != nil {
		t.Fatalf("second RunCardCommandAsHuman: %v", err)
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
	// The SECOND call's own submitted instruction ("second") must come
	// back, not the first request's ("first") or nothing at all.
	if second.Instruction != "second" {
		t.Errorf("second.Instruction = %q, want %q (the occupied call's own submitted instruction, echoed back)", second.Instruction, "second")
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

// TestRunCardCommandAsHuman_SpeccedJSONChildOnly_StillLaunches pins that a
// specced/open task_triage JSON child with NO live task row yet does NOT
// block a card command.
//
// docs/plans/card-next-step-and-timeline.md §3.2 keeps the two constraints
// apart: a specced child occupies the NEXT-STEP SPEC slot, which explicitly
// "仕様を作る対話・判断と共存できる", while a command occupies the EXECUTION
// slot ("Go による作業 task、コマンドが作る task、対話 session の合計"). A
// spec waiting for Go is not an execution — nothing is running — so pressing
// Discuss to talk that spec over before pressing Go must work.
//
// Treating it as an occupant also produced a dead end: §4.4 has the occupied
// response point at "現在の実行", and a JSON child has no task row to point
// at, so the caller got Occupied with an empty TargetKind/TargetID and
// nothing to follow.
func TestRunCardCommandAsHuman_SpeccedJSONChildOnly_StillLaunches(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.TaskTriage.(*orchestrator.TaskRepository)
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{
		TaskID: card.ID,
		Detail: []byte(`{"children":[{"id":"ch_00","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}]}`),
	}); err != nil {
		t.Fatalf("seed task_triage: %v", err)
	}

	result, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "go do it")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman: %v", err)
	}
	if result.Occupied {
		t.Fatalf("Occupied = true, want false — a specced child holds the spec slot, not the execution slot (target was %q/%q)",
			result.TargetKind, result.TargetID)
	}
	if result.LauncherJobID == "" {
		t.Error("LauncherJobID is empty, want the launcher exec job id")
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1", len(exec.calls))
	}
}

// TestRunCardCommandAsHuman_DispatchFailure_ReleasesSlot pins that a launcher
// dispatch failure fails the claimed request (retry-able) rather than
// leaving the slot stuck forever.
func TestRunCardCommandAsHuman_DispatchFailure_ReleasesSlot(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	exec.failNext = 1

	_, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
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
	second, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman after release: %v", err)
	}
	if second.Occupied {
		t.Fatal("Occupied = true, want the released slot to accept a fresh claim")
	}
}

// TestRunCardCommandAsHuman_DispatchFailure_RecordsSelfLog pins that
// releasing the slot on a human command's dispatch failure also
// self-records a command_failed entry (this call's request has an empty
// CauseID, i.e. human origin — TestFailCardRequest_RecordsCommandFailedOnCard_EventOrigin
// in internal/orchestrator covers the event-origin case at the leaf level).
func TestRunCardCommandAsHuman_DispatchFailure_RecordsSelfLog(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	exec.failNext = 1

	if _, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", ""); err == nil {
		t.Fatal("want an error when dispatch fails")
	}

	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	actions, err := repo.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != "command_failed" {
		t.Fatalf("actions = %+v, want exactly one command_failed self-record", actions)
	}
}

// TestRunCardCommandAsHuman_TerminalCard_Returns409 pins that a manual card
// command is rejected against a done/dropped card — mirrors acceptGo's own
// parked/working guard (workflow_card.go), so manual and automatic dispatch
// agree: a terminal card never takes a fresh execution slot, even by hand.
func TestRunCardCommandAsHuman_TerminalCard_Returns409(t *testing.T) {
	for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped} {
		t.Run(string(status), func(t *testing.T) {
			svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
				"review": {Label: "Run", Run: "echo hi"},
			}))
			repo := svc.Tasks.(*orchestrator.TaskRepository)
			card.Status = status
			if err := repo.UpdateTask(card); err != nil {
				t.Fatalf("UpdateTask: %v", err)
			}

			_, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
			if err == nil {
				t.Fatalf("want error for a %s (terminal) card", status)
			}
			var se *StatusError
			if !errors.As(err, &se) || se.Code != 409 {
				t.Fatalf("err = %v, want a 409 StatusError", err)
			}
			if len(exec.calls) != 0 {
				t.Fatalf("StartExec calls = %d, want 0 — must not dispatch against a terminal card", len(exec.calls))
			}
			rows, err := repo.ListCardRequestsByCard(card.ID)
			if err != nil {
				t.Fatalf("ListCardRequestsByCard: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("card_requests rows = %d, want 0 — no slot should be claimed against a terminal card", len(rows))
			}
		})
	}
}

// TestCardWorkChildOccupantTx_TerminalCard_Returns409 pins the terminal-card
// guard as it actually runs: cardWorkChildOccupantTx re-reads the card FRESH
// from tx and rejects a terminal card there, mirroring acceptGo's own in-Tx
// re-verify (workflow_card.go). This is RunCardCommandAsHuman's ONLY status
// guard — a stale read taken before the reservation Tx opens (a concurrent
// complete/drop could otherwise land in that gap) would defeat the whole
// point of re-checking inside the same transaction that claims the slot.
func TestCardWorkChildOccupantTx_TerminalCard_Returns409(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.Tasks.(*orchestrator.TaskRepository)
	card.Status = orchestrator.TaskStatusDone
	if err := repo.UpdateTask(card); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	var innerErr error
	if err := svc.Tx.WithinTx(func(tx TxStore) error {
		_, _, err := cardWorkChildOccupantTx(tx, card.ID)
		innerErr = err
		return err
	}); err == nil {
		t.Fatal("want an error re-checking a terminal card's fresh status inside the Tx")
	}
	var se *StatusError
	if !errors.As(innerErr, &se) || se.Code != 409 {
		t.Fatalf("err = %v, want a 409 StatusError", innerErr)
	}
}

// TestCardWorkChildOccupantTx_WorkingCard_Succeeds pins the OTHER side of
// this same guard directly: a `working` card (not just `parked`) must not
// be rejected — tightening the guard to `!= parked` (dropping the `working`
// half) would leave this uncaught.
func TestCardWorkChildOccupantTx_WorkingCard_Succeeds(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.Tasks.(*orchestrator.TaskRepository)
	card.Status = orchestrator.TaskStatusWorking
	if err := repo.UpdateTask(card); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	if err := svc.Tx.WithinTx(func(tx TxStore) error {
		_, _, err := cardWorkChildOccupantTx(tx, card.ID)
		return err
	}); err != nil {
		t.Fatalf("cardWorkChildOccupantTx: %v, want success for a working card", err)
	}
}

// TestRunCardCommandAsHuman_CorruptTaskTriageDetail_FailsClosed pins that a
// task_triage detail blob DetailOpenSlotChildID cannot parse propagates as
// an error rather than reading as "no JSON occupant" — cardWorkChildOccupantTx
// must fail closed here the same way cardSlotOccupied does (workflow_card.go),
// or a corrupted blob would silently let a second execution through
// alongside whatever the JSON side actually still holds.
func TestRunCardCommandAsHuman_CorruptTaskTriageDetail_FailsClosed(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.TaskTriage.(*orchestrator.TaskRepository)
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{
		TaskID: card.ID,
		Detail: []byte(`not valid json`),
	}); err != nil {
		t.Fatalf("seed task_triage: %v", err)
	}

	_, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
	if err == nil {
		t.Fatal("want an error for a corrupt task_triage detail blob, not a silent fail-open")
	}
	if len(exec.calls) != 0 {
		t.Fatalf("StartExec calls = %d, want 0 — must not dispatch when occupancy could not be determined", len(exec.calls))
	}
}

// TestRunCardCommandAsHuman_LiveGoChild_ReturnsLinkWithoutDispatching pins that a
// card command must not dispatch alongside an already-running Go work
// child — the two share one execution slot.
func TestRunCardCommandAsHuman_LiveGoChild_ReturnsLinkWithoutDispatching(t *testing.T) {
	svc, exec, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.Tasks.(*orchestrator.TaskRepository)
	child := &orchestrator.Task{
		ProjectID: "proj-1", ParentID: card.ID, Type: orchestrator.TaskTypeExecution,
		Ref: "child-1", Exec: &orchestrator.ExecAttrs{Behavior: "executor"},
	}
	if err := repo.CreateTask(child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	result, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", "")
	if err != nil {
		t.Fatalf("RunCardCommandAsHuman: %v", err)
	}
	if !result.Occupied {
		t.Fatal("Occupied = false, want true — a live Go child must block the command")
	}
	if len(exec.calls) != 0 {
		t.Fatalf("StartExec calls = %d, want 0 — must not dispatch alongside a live Go child", len(exec.calls))
	}
	// The occupant is a REAL live task row (child.ID), not the empty string
	// and not any task_triage JSON id — a caller following TargetID with
	// GET /api/tasks/<id> must land on the actual running child.
	if result.TargetKind != orchestrator.CardRequestTargetKindTask || result.TargetID != child.ID {
		t.Errorf("TargetKind/TargetID = %q/%q, want %q/%q (the live child's real task id)",
			result.TargetKind, result.TargetID, orchestrator.CardRequestTargetKindTask, child.ID)
	}
	rows, err := repo.ListCardRequestsByCard(card.ID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("card_requests rows = %d, want 0 — no request should be created while a Go child occupies the slot", len(rows))
	}
}
