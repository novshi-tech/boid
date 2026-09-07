package orchestrator_test

// Call-site-level coverage: proves FinishCardRequest/FailCardRequest's
// self-record (card_request_self_record_test.go pins the leaf behavior)
// actually lands when reached through each of card_request_release.go's
// own callers, not just when called directly.

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestReconcileCardRequestSlots_TaskContinuation_RecordsSelfLog(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	doneTask := newTestExecutionTask(t, d, "task-done", "proj-1", orchestrator.TaskStatusDone)
	attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, doneTask)

	if _, err := orchestrator.ReconcileCardRequestSlots(d.Conn); err != nil {
		t.Fatalf("ReconcileCardRequestSlots: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != "command_finished" {
		t.Fatalf("actions = %+v, want exactly one command_finished self-record", actions)
	}
}

func TestReconcileCardRequestSlots_AbortedTaskReleasesAsFailed_RecordsSelfLog(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	abortedTask := newTestExecutionTask(t, d, "task-aborted", "proj-1", orchestrator.TaskStatusAborted)
	attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, abortedTask)

	if _, err := orchestrator.ReconcileCardRequestSlots(d.Conn); err != nil {
		t.Fatalf("ReconcileCardRequestSlots: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != "command_failed" {
		t.Fatalf("actions = %+v, want exactly one command_failed self-record", actions)
	}
}

// TestRecoverLaunchingCardRequests_NoContinuationFound_RecordsSelfLog covers
// attachFoundContinuationOrFail's FailCardRequest call reached via the
// daemon-startup recovery scan (a distinct caller from
// ReconcileLaunchingCardRequests, even though both funnel through the same
// shared helper).
func TestRecoverLaunchingCardRequests_NoContinuationFound_RecordsSelfLog(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-crashed-3"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	insertTestJob(t, d, "launcher-crashed-3", "proj-1", "hook", "failed", req.ID)

	if _, err := orchestrator.RecoverLaunchingCardRequests(d.Conn); err != nil {
		t.Fatalf("RecoverLaunchingCardRequests: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != "command_failed" {
		t.Fatalf("actions = %+v, want exactly one command_failed self-record", actions)
	}
}

// TestReconcileLaunchingCardRequests_LauncherTerminatedNoContinuation_RecordsSelfLog
// covers attachFoundContinuationOrFail's FailCardRequest call reached via
// the PERIODIC self-heal (ReconcileLaunchingCardRequests) — gated on the
// launcher job's own terminal status, a distinct precondition from
// RecoverLaunchingCardRequests' unconditional startup scan above, even
// though both share the same inner helper.
func TestReconcileLaunchingCardRequests_LauncherTerminatedNoContinuation_RecordsSelfLog(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-terminated"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	insertTestJob(t, d, "launcher-terminated", "proj-1", "exec", "completed", req.ID)

	if _, err := orchestrator.ReconcileLaunchingCardRequests(d.Conn); err != nil {
		t.Fatalf("ReconcileLaunchingCardRequests: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != "command_failed" {
		t.Fatalf("actions = %+v, want exactly one command_failed self-record", actions)
	}
}

// TestRecoverLaunchingCardRequests_GoRow_NoContinuationFound_NoSelfRecord is
// the Go-exclusion guard exercised through a REAL caller (not the leaf
// FailCardRequest test): RecoverLaunchingCardRequests deliberately does NOT
// skip CardRequestCommandKeyGo rows (unlike ReconcileLaunchingCardRequests —
// see that function's own doc comment on why a crashed daemon can still
// leave a Go reservation stuck launching), so this is a real production path
// that reaches FailCardRequest with a Go-keyed row.
func TestRecoverLaunchingCardRequests_GoRow_NoContinuationFound_NoSelfRecord(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{
		CardID: cardID, CommandKey: orchestrator.CardRequestCommandKeyGo,
		Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "go:crashed-launcher",
	}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	// Deliberately no jobs row for "go:crashed-launcher" — acceptGo's Go
	// reservation has no real launcher job (task creation is synchronous
	// in-process), so the reverse-lookup always comes up empty for it.

	outcomes, err := orchestrator.RecoverLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("RecoverLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want one failed outcome for %q (the row itself IS still failed — only the self-record is skipped)", outcomes, req.ID)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none — a Go reservation's release must not self-record", actions)
	}
}

func TestReleaseCardRequestForTerminalTargetWithCard_Success_RecordsSelfLog(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	task := newTestExecutionTask(t, d, "task-1", "proj-1", orchestrator.TaskStatusDone)
	attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, task)

	if found, _, err := orchestrator.ReleaseCardRequestForTerminalTargetWithCard(d.Conn, orchestrator.CardRequestTargetKindTask, task, true); err != nil || !found {
		t.Fatalf("ReleaseCardRequestForTerminalTargetWithCard: (%v, %v)", found, err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != "command_finished" {
		t.Fatalf("actions = %+v, want exactly one command_finished self-record", actions)
	}
}

func TestReleaseCardRequestForTerminalTargetWithCard_Failure_RecordsSelfLog(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	task := newTestExecutionTask(t, d, "task-1", "proj-1", orchestrator.TaskStatusAborted)
	attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, task)

	if found, _, err := orchestrator.ReleaseCardRequestForTerminalTargetWithCard(d.Conn, orchestrator.CardRequestTargetKindTask, task, false); err != nil || !found {
		t.Fatalf("ReleaseCardRequestForTerminalTargetWithCard: (%v, %v)", found, err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != "command_failed" {
		t.Fatalf("actions = %+v, want exactly one command_failed self-record", actions)
	}
}

// TestForceReleaseCardRequest_DoesNotSelfRecord pins that
// ForceReleaseCardRequest does not self-record — it calls the shared
// internal failCardRequest helper directly, bypassing FailCardRequest's own
// wrapper entirely.
func TestForceReleaseCardRequest_DoesNotSelfRecord(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")

	if _, err := orchestrator.ForceReleaseCardRequest(d.Conn, req.ID, "operator stop"); err != nil {
		t.Fatalf("ForceReleaseCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none (ForceReleaseCardRequest does not self-record)", actions)
	}
}
