package orchestrator_test

// FinishCardRequest/FailCardRequest's own self-record of a card_request's
// terminal outcome onto its card's action log — the leaf-level behavior.
// Call-site-level coverage (proving every actual caller gets this for
// free) lives alongside each caller's own existing test file.

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

type cardRequestSelfRecordSibling struct {
	ID         string `json:"id"`
	CommandKey string `json:"command_key"`
}

type cardRequestSelfRecordPayload struct {
	RequestID           string                         `json:"request_id"`
	CommandKey          string                         `json:"command_key"`
	LaunchedLabel       string                         `json:"launched_label"`
	LauncherJobID       string                         `json:"launcher_job_id"`
	TargetKind          string                         `json:"target_kind"`
	TargetID            string                         `json:"target_id"`
	Origin              string                         `json:"origin"`
	CauseID             string                         `json:"cause_id"`
	Result              string                         `json:"result"`
	Error               string                         `json:"error"`
	Reason              string                         `json:"reason"`
	ForceFailedSiblings []cardRequestSelfRecordSibling `json:"force_failed_siblings"`
}

func decodeSelfRecordPayload(t *testing.T, raw []byte) cardRequestSelfRecordPayload {
	t.Helper()
	var p cardRequestSelfRecordPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode self-record payload: %v (raw=%s)", err, raw)
	}
	return p
}

// launchedCardRequest creates, claims, and attaches a card_requests row for
// cardID — the common setup every self-record test below needs before it
// can call FinishCardRequest/FailCardRequest from "attached".
func launchedCardRequest(t *testing.T, d *db.DB, cardID, commandKey, causeID, launcherJobID, targetKind, targetID string) *orchestrator.CardRequest {
	t.Helper()
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: commandKey, CauseID: causeID}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	def := orchestrator.CardRequestDefinition{CommandKey: commandKey, Label: "Run Review", Run: "python3 scripts/review.py"}
	primary, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, launcherJobID, def)
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, primary.ID, targetKind, targetID); err != nil {
		t.Fatalf("AttachCardRequest: %v", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, primary.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	return got
}

func TestFinishCardRequest_RecordsCommandFinishedOnCard_HumanOrigin(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, "review", "", "launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")

	if err := orchestrator.FinishCardRequest(d.Conn, req.ID, "no further action"); err != nil {
		t.Fatalf("FinishCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(actions))
	}
	a := actions[0]
	if a.Type != "command_finished" {
		t.Errorf("Type = %q, want command_finished", a.Type)
	}
	if a.Actor != orchestrator.ActorDaemon {
		t.Errorf("Actor = %q, want %q", a.Actor, orchestrator.ActorDaemon)
	}
	p := decodeSelfRecordPayload(t, a.Payload)
	if p.RequestID != req.ID {
		t.Errorf("RequestID = %q, want %q", p.RequestID, req.ID)
	}
	if p.CommandKey != "review" {
		t.Errorf("CommandKey = %q, want review", p.CommandKey)
	}
	if p.LaunchedLabel != "Run Review" {
		t.Errorf("LaunchedLabel = %q, want %q (the queued->launching snapshot, not a re-read of project.yaml)", p.LaunchedLabel, "Run Review")
	}
	if p.TargetKind != orchestrator.CardRequestTargetKindTask || p.TargetID != "task-1" {
		t.Errorf("target = (%q, %q), want (task, task-1)", p.TargetKind, p.TargetID)
	}
	if p.LauncherJobID != "launcher-1" {
		t.Errorf("LauncherJobID = %q, want %q", p.LauncherJobID, "launcher-1")
	}
	if p.Origin != "human" {
		t.Errorf("Origin = %q, want human (empty cause_id)", p.Origin)
	}
	if p.CauseID != "" {
		t.Errorf("CauseID = %q, want empty (human origin)", p.CauseID)
	}
	if p.Result != "no further action" {
		t.Errorf("Result = %q, want %q", p.Result, "no further action")
	}
	if p.Error != "" {
		t.Errorf("Error = %q, want empty on a finish", p.Error)
	}
}

func TestFailCardRequest_RecordsCommandFailedOnCard_EventOrigin(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, "review", "signal-42", "launcher-1", orchestrator.CardRequestTargetKindSession, "job-1")

	if err := orchestrator.FailCardRequest(d.Conn, req.ID, "continuation ended without success"); err != nil {
		t.Fatalf("FailCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(actions))
	}
	a := actions[0]
	if a.Type != "command_failed" {
		t.Errorf("Type = %q, want command_failed", a.Type)
	}
	p := decodeSelfRecordPayload(t, a.Payload)
	if p.TargetKind != orchestrator.CardRequestTargetKindSession || p.TargetID != "job-1" {
		t.Errorf("target = (%q, %q), want (session, job-1)", p.TargetKind, p.TargetID)
	}
	if p.Origin != "event" {
		t.Errorf("Origin = %q, want event (non-empty cause_id)", p.Origin)
	}
	if p.CauseID != "signal-42" {
		t.Errorf("CauseID = %q, want %q", p.CauseID, "signal-42")
	}
	if p.Error != "continuation ended without success" {
		t.Errorf("Error = %q, want %q", p.Error, "continuation ended without success")
	}
	if p.Result != "" {
		t.Errorf("Result = %q, want empty on a failure", p.Result)
	}
}

// TestFailCardRequest_NeverLaunched_StillRecords pins that a request failed
// straight from queued (dispatchQueuedCardRequest's "command no longer
// declared in project.yaml" path — it never reaches launching) still gets a
// self-record, just with an empty launched_label since it was never
// claimed.
func TestFailCardRequest_NeverLaunched_StillRecords(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "vanished"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}

	if err := orchestrator.FailCardRequest(d.Conn, req.ID, "command no longer declared"); err != nil {
		t.Fatalf("FailCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(actions))
	}
	p := decodeSelfRecordPayload(t, actions[0].Payload)
	if p.CommandKey != "vanished" {
		t.Errorf("CommandKey = %q, want vanished", p.CommandKey)
	}
	if p.LaunchedLabel != "" {
		t.Errorf("LaunchedLabel = %q, want empty (never claimed)", p.LaunchedLabel)
	}
}

// ---- Go exclusion: a Go reservation self-records via child_closed instead ----

func TestFinishCardRequest_GoCommandKey_NoSelfRecord(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, orchestrator.CardRequestCommandKeyGo, "", "go:launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")

	if err := orchestrator.FinishCardRequest(d.Conn, req.ID, "child dispatched"); err != nil {
		t.Fatalf("FinishCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %d, want 0 — Go's own reservation must not self-record (child_closed already covers it)", len(actions))
	}
}

func TestFailCardRequest_GoCommandKey_NoSelfRecord(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, orchestrator.CardRequestCommandKeyGo, "", "go:launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")

	if err := orchestrator.FailCardRequest(d.Conn, req.ID, "reservation released on error"); err != nil {
		t.Fatalf("FailCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %d, want 0 — Go's own reservation release must not self-record", len(actions))
	}
}

// ---- same-transaction atomicity ----

// TestFinishCardRequest_SelfRecordFailure_RollsBackTerminalTransitionToo
// breaks the self-record INSERT (by dropping the actions table out from
// under it) and pins that the whole FinishCardRequest call fails and, when
// run inside a transaction, rolls back the card_requests status change
// alongside it. If the self-record write were instead best-effort
// (logged and swallowed, the way IngestActionSignal treats its own
// failures), this test goes red: FinishCardRequest would return nil and the
// row would show finished despite no self-record ever landing.
func TestFinishCardRequest_SelfRecordFailure_RollsBackTerminalTransitionToo(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, "review", "", "launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")

	if _, err := d.Conn.Exec(`DROP TABLE actions`); err != nil {
		t.Fatalf("drop actions table: %v", err)
	}

	txErr := db.InTxDB(d.Conn, func(tx db.DBTX) error {
		return orchestrator.FinishCardRequest(tx, req.ID, "no further action")
	})
	if txErr == nil {
		t.Fatal("FinishCardRequest inside a tx with a broken self-record write: want an error, got nil")
	}

	// Recreate actions so GetCardRequest's own machinery (unaffected) can be
	// read back cleanly, then confirm the card_requests row itself rolled
	// back to its pre-call state.
	if _, err := d.Conn.Exec(`CREATE TABLE actions (
		id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), type TEXT NOT NULL,
		payload TEXT NOT NULL DEFAULT '{}', created_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatalf("recreate actions table: %v", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached {
		t.Fatalf("Status = %q, want still attached (the terminal UPDATE must have rolled back with the failed self-record)", got.Status)
	}
}

func TestFailCardRequest_SelfRecordFailure_RollsBackTerminalTransitionToo(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, "review", "", "launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")

	if _, err := d.Conn.Exec(`DROP TABLE actions`); err != nil {
		t.Fatalf("drop actions table: %v", err)
	}

	txErr := db.InTxDB(d.Conn, func(tx db.DBTX) error {
		return orchestrator.FailCardRequest(tx, req.ID, "continuation ended without success")
	})
	if txErr == nil {
		t.Fatal("FailCardRequest inside a tx with a broken self-record write: want an error, got nil")
	}

	if _, err := d.Conn.Exec(`CREATE TABLE actions (
		id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), type TEXT NOT NULL,
		payload TEXT NOT NULL DEFAULT '{}', created_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatalf("recreate actions table: %v", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached {
		t.Fatalf("Status = %q, want still attached (the terminal UPDATE must have rolled back with the failed self-record)", got.Status)
	}
}

// ---- GC interaction: the card_requests row is disposable, the card's own
// action log survives it ----

// TestGCCardRequests_DeletesRowButActionsSurvive is the requirement-5 real-
// DB check: GCCardRequests deletes the card_requests row unconditionally by
// its own age, independent of the card task's own (non-terminal) status —
// the self-recorded action on the card's task_id is untouched, because
// GCTasks (which deletes actions) only ever targets terminal, aged TASK
// rows and this card is still parked.
func TestGCCardRequests_DeletesRowButActionsSurvive(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, "review", "", "launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")
	if err := orchestrator.FinishCardRequest(d.Conn, req.ID, "no further action"); err != nil {
		t.Fatalf("FinishCardRequest: %v", err)
	}

	n, err := orchestrator.GCCardRequests(d.Conn, 0, false)
	if err != nil {
		t.Fatalf("GCCardRequests: %v", err)
	}
	if n != 1 {
		t.Fatalf("GCCardRequests deleted = %d, want 1", n)
	}
	if _, err := orchestrator.GetCardRequest(d.Conn, req.ID); err == nil {
		t.Fatal("GetCardRequest after GC: want ErrCardRequestNotFound, got nil error")
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask after GC: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != "command_finished" {
		t.Fatalf("actions after GCCardRequests = %+v, want the command_finished self-record still present", actions)
	}
}

// TestGCTasks_TerminalOldCard_TakesTheSelfRecordActionsWithIt is the OTHER
// half: once the card itself is terminal and old enough for GCTasks, its
// own action log — including any command_finished/command_failed self-
// records — is deleted along with it, the same fate as child_closed's.
func TestGCTasks_TerminalOldCard_TakesTheSelfRecordActionsWithIt(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, "review", "", "launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")
	if err := orchestrator.FinishCardRequest(d.Conn, req.ID, "no further action"); err != nil {
		t.Fatalf("FinishCardRequest: %v", err)
	}

	card, err := orchestrator.GetTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	card.Status = orchestrator.TaskStatusDone
	if err := orchestrator.UpdateTask(d.Conn, card); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	if _, err := orchestrator.GCTasks(d.Conn, []string{string(orchestrator.TaskStatusDone)}, 0, false); err != nil {
		t.Fatalf("GCTasks: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask after GCTasks: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions after GCTasks on the terminal card = %+v, want none (the whole card is gone)", actions)
	}
}

// ---- shape facts a reader of the self-record actions must know ----

// TestFinishCardRequest_FoldedSiblings_OnlyHeadSelfRecords pins that when a
// claim folds several queued requests into one head, finishing the head
// writes exactly ONE self-record — the folded siblings never got their own
// launched snapshot (they were absorbed into the head's single run), so
// they get no action of their own either.
func TestFinishCardRequest_FoldedSiblings_OnlyHeadSelfRecords(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	first := &orchestrator.CardRequest{CardID: cardID, CauseID: "cause-1"}
	second := &orchestrator.CardRequest{CardID: cardID, CauseID: "cause-2"}
	if err := orchestrator.CreateCardRequest(d.Conn, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := orchestrator.CreateCardRequest(d.Conn, second); err != nil {
		t.Fatalf("create second: %v", err)
	}
	primary, folded, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "launcher-1", orchestrator.CardRequestDefinition{CommandKey: "review"})
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests: %v", err)
	}
	if len(folded) != 1 {
		t.Fatalf("folded = %d, want 1", len(folded))
	}
	if err := orchestrator.AttachCardRequest(d.Conn, primary.ID, orchestrator.CardRequestTargetKindTask, "task-1"); err != nil {
		t.Fatalf("AttachCardRequest: %v", err)
	}

	if err := orchestrator.FinishCardRequest(d.Conn, primary.ID, "done"); err != nil {
		t.Fatalf("FinishCardRequest: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %d, want exactly 1 (the folded sibling must not get its own self-record)", len(actions))
	}
	p := decodeSelfRecordPayload(t, actions[0].Payload)
	if p.RequestID != primary.ID {
		t.Errorf("RequestID = %q, want the head %q, not the folded sibling", p.RequestID, primary.ID)
	}
}

// TestFailCardRequest_RetryThenFailAgain_TwoSelfRecordsSameRequestID pins
// that RetryCardRequest keeps the same request_id, so a request that fails,
// gets retried, and fails again accumulates TWO terminal self-records
// sharing one request_id — a reader must not assume request_id uniquely
// identifies one action row.
func TestFailCardRequest_RetryThenFailAgain_TwoSelfRecordsSameRequestID(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := launchedCardRequest(t, d, cardID, "review", "", "launcher-1", orchestrator.CardRequestTargetKindTask, "task-1")

	if err := orchestrator.FailCardRequest(d.Conn, req.ID, "first failure"); err != nil {
		t.Fatalf("FailCardRequest (1st): %v", err)
	}
	if err := orchestrator.RetryCardRequest(d.Conn, req.ID); err != nil {
		t.Fatalf("RetryCardRequest: %v", err)
	}
	if _, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "launcher-2", orchestrator.CardRequestDefinition{CommandKey: "review"}); err != nil {
		t.Fatalf("ClaimQueuedCardRequests (retry): %v", err)
	}
	if err := orchestrator.FailCardRequest(d.Conn, req.ID, "second failure"); err != nil {
		t.Fatalf("FailCardRequest (2nd): %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want 2 (both terminal outcomes for the same retried request_id)", len(actions))
	}
	for _, a := range actions {
		p := decodeSelfRecordPayload(t, a.Payload)
		if p.RequestID != req.ID {
			t.Errorf("RequestID = %q, want %q on both records", p.RequestID, req.ID)
		}
	}
}

// ---- origin derivation stays aligned with `boid card context`'s own rule
// (internal/server/boid_executor_card_context.go's cardRequestOrigin) ----

func TestCardRequestOrigin_MatchesCauseIDEmptiness(t *testing.T) {
	if got := orchestrator.CardRequestOrigin(""); got != orchestrator.CardRequestOriginHuman {
		t.Errorf("CardRequestOrigin(\"\") = %q, want %q", got, orchestrator.CardRequestOriginHuman)
	}
	if got := orchestrator.CardRequestOrigin("signal-1"); got != orchestrator.CardRequestOriginEvent {
		t.Errorf("CardRequestOrigin(non-empty) = %q, want %q", got, orchestrator.CardRequestOriginEvent)
	}
}
