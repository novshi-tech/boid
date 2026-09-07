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

// TestForceReleaseCardRequest_RecordsSelfLog pins that ForceReleaseCardRequest
// self-records the operator's reason as a command_force_released action —
// otherwise "why did this card stop auto-dispatching" has no durable answer
// once card_requests is GC'd, since the force-release barrier row itself
// (card_force_release_barriers) carries no reason and nothing reads it.
func TestForceReleaseCardRequest_RecordsSelfLog(t *testing.T) {
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
	if len(actions) != 1 || actions[0].Type != "command_force_released" {
		t.Fatalf("actions = %+v, want exactly one command_force_released self-record", actions)
	}
	p := decodeSelfRecordPayload(t, actions[0].Payload)
	if p.RequestID != req.ID {
		t.Errorf("RequestID = %q, want %q", p.RequestID, req.ID)
	}
	if p.Reason != "operator stop" {
		t.Errorf("Reason = %q, want %q", p.Reason, "operator stop")
	}
	if p.TargetKind != orchestrator.CardRequestTargetKindTask || p.TargetID != "task-1" {
		t.Errorf("target = (%q, %q), want (task, task-1) — force-release does not clear target_kind/target_id", p.TargetKind, p.TargetID)
	}
}

// TestForceReleaseCardRequest_RecordsForceFailedSiblings pins that a folded
// sibling force-release also force-fails gets named in the self-record's
// own payload — the barrier row and the card_requests rows themselves both
// disappear from GC, so this is the only durable place that association
// survives.
func TestForceReleaseCardRequest_RecordsForceFailedSiblings(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	head := &orchestrator.CardRequest{CardID: cardID}
	sibling := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, head); err != nil {
		t.Fatalf("create head: %v", err)
	}
	if err := orchestrator.CreateCardRequest(d.Conn, sibling); err != nil {
		t.Fatalf("create sibling: %v", err)
	}
	primary, folded, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "launcher-1", orchestrator.CardRequestDefinition{CommandKey: "review"})
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests: %v", err)
	}
	if len(folded) != 1 {
		t.Fatalf("folded = %d, want 1", len(folded))
	}

	if _, err := orchestrator.ForceReleaseCardRequest(d.Conn, primary.ID, "operator stop"); err != nil {
		t.Fatalf("ForceReleaseCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(actions))
	}
	p := decodeSelfRecordPayload(t, actions[0].Payload)
	if len(p.ForceFailedSiblings) != 1 || p.ForceFailedSiblings[0].ID != sibling.ID {
		t.Fatalf("ForceFailedSiblings = %+v, want exactly [%q]", p.ForceFailedSiblings, sibling.ID)
	}
}

// TestForceReleaseCardRequest_GoCommandKey_NoSelfRecord is the Go-exclusion
// guard applied to force-release too — an operator force-releasing a stuck
// Go reservation must not self-record (child_closed already covers a
// Go-dispatched child's own terminal outcome).
func TestForceReleaseCardRequest_GoCommandKey_NoSelfRecord(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{
		CardID: cardID, CommandKey: orchestrator.CardRequestCommandKeyGo,
		Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "go:launcher-1",
	}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}

	if _, err := orchestrator.ForceReleaseCardRequest(d.Conn, req.ID, "operator stop"); err != nil {
		t.Fatalf("ForceReleaseCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none — force-releasing a Go reservation must not self-record", actions)
	}
}
