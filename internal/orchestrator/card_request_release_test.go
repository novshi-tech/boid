package orchestrator_test

// card_requests の枠解放・復旧走査 (card_request_release.go) のテスト。

import (
	"errors"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// newTestExecutionTask creates a minimal execution-type task in the given
// status, standing in for a card command's task continuation.
func newTestExecutionTask(t *testing.T, d *db.DB, id, projectID string, status orchestrator.TaskStatus) string {
	t.Helper()
	if _, err := orchestrator.GetProject(d.Conn, projectID); err != nil {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: projectID, WorkDir: "/tmp/" + projectID}); err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	task := &orchestrator.Task{ID: id, ProjectID: projectID, Type: orchestrator.TaskTypeExecution, Status: status, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create execution task: %v", err)
	}
	return task.ID
}

// insertTestJob inserts a minimal jobs row directly (dispatcher owns the
// real CreateJob path; this package can't import dispatcher — see
// card_request_release.go's own doc comment on that constraint).
func insertTestJob(t *testing.T, d *db.DB, id, projectID, status, cardRequestID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := d.Conn.Exec(
		`INSERT INTO jobs (id, project_id, handler_id, role, status, card_request_id, created_at, updated_at) VALUES (?, ?, '', 'hook', ?, ?, ?, ?)`,
		id, projectID, status, cardRequestID, now, now,
	); err != nil {
		t.Fatalf("insert test job: %v", err)
	}
}

func attachedCardRequest(t *testing.T, d *db.DB, cardID, launcherJobID, targetKind, targetID string) *orchestrator.CardRequest {
	t.Helper()
	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: launcherJobID}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create launching card request: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, targetKind, targetID); err != nil {
		t.Fatalf("attach card request: %v", err)
	}
	return req
}

func TestReconcileCardRequestSlots_TaskContinuation(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	doneTask := newTestExecutionTask(t, d, "task-done", "proj-1", orchestrator.TaskStatusDone)
	reqDone := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, doneTask)

	outcomes, err := orchestrator.ReconcileCardRequestSlots(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileCardRequestSlots: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != reqDone.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFinished) {
		t.Fatalf("outcomes = %+v, want one finished outcome for %q", outcomes, reqDone.ID)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, reqDone.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFinished {
		t.Errorf("Status = %q, want finished", got.Status)
	}
}

func TestReconcileCardRequestSlots_AbortedTaskReleasesAsFailed(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	abortedTask := newTestExecutionTask(t, d, "task-aborted", "proj-1", orchestrator.TaskStatusAborted)
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, abortedTask)

	if _, err := orchestrator.ReconcileCardRequestSlots(d.Conn); err != nil {
		t.Fatalf("ReconcileCardRequestSlots: %v", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("Status = %q, want failed (aborted continuation is not a successful judgment)", got.Status)
	}
	if n, cerr := orchestrator.CountActiveCardRequests(d.Conn, cardID); cerr != nil || n != 0 {
		t.Fatalf("CountActiveCardRequests after release = (%d, %v), want (0, nil)", n, cerr)
	}
}

// TestReconcileCardRequestSlots_LiveTaskUntouched pins that a still-executing
// task continuation is left attached — the whole point of confirming
// termination rather than releasing on a timer.
func TestReconcileCardRequestSlots_LiveTaskUntouched(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	liveTask := newTestExecutionTask(t, d, "task-live", "proj-1", orchestrator.TaskStatusExecuting)
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, liveTask)

	outcomes, err := orchestrator.ReconcileCardRequestSlots(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileCardRequestSlots: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want none (task still executing)", outcomes)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached {
		t.Errorf("Status = %q, want still attached", got.Status)
	}
}

// TestReconcileCardRequestSlots_SessionReleasesOnJobTerminalNotHookJob pins
// §4.4's "Run の hook job だけが終わっても解放しない" — only the job named by
// target_id (the session's own job) releases the slot; an unrelated
// terminal job for the same card does nothing.
func TestReconcileCardRequestSlots_SessionReleasesOnJobTerminalNotHookJob(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	insertTestJob(t, d, "hook-job-1", "proj-1", "completed", "")
	insertTestJob(t, d, "session-job-1", "proj-1", "running", "")
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindSession, "session-job-1")

	outcomes, err := orchestrator.ReconcileCardRequestSlots(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileCardRequestSlots: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want none (the session's OWN job is still running, an unrelated hook job finishing doesn't count)", outcomes)
	}

	if _, err := d.Conn.Exec(`UPDATE jobs SET status = 'completed' WHERE id = 'session-job-1'`); err != nil {
		t.Fatalf("mark session job completed: %v", err)
	}
	outcomes, err = orchestrator.ReconcileCardRequestSlots(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileCardRequestSlots (2nd pass): %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFinished) {
		t.Fatalf("outcomes = %+v, want one finished outcome for %q", outcomes, req.ID)
	}
}

func TestReconcileCardRequestSlots_FailedSessionJobReleasesAsFailed(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	insertTestJob(t, d, "session-job-2", "proj-1", "failed", "")
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindSession, "session-job-2")

	if _, err := orchestrator.ReconcileCardRequestSlots(d.Conn); err != nil {
		t.Fatalf("ReconcileCardRequestSlots: %v", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
}

func TestRecoverLaunchingCardRequests_ReattachesFoundContinuation(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-crashed"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	// The launcher's own job also carries the request's card_request_id
	// (it needs it to call `boid card context` / `boid agent start`) — the
	// recovery scan must not mistake it for the continuation it created.
	insertTestJob(t, d, "launcher-crashed", "proj-1", "failed", req.ID)
	insertTestJob(t, d, "session-job-3", "proj-1", "running", req.ID)

	outcomes, err := orchestrator.RecoverLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("RecoverLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusAttached) {
		t.Fatalf("outcomes = %+v, want one attached outcome for %q", outcomes, req.ID)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached || got.TargetKind != orchestrator.CardRequestTargetKindSession || got.TargetID != "session-job-3" {
		t.Fatalf("got = %+v, want attached to session-job-3", got)
	}
}

func TestRecoverLaunchingCardRequests_NoContinuationFoundFailsRetryable(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-crashed-2"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	insertTestJob(t, d, "launcher-crashed-2", "proj-1", "failed", req.ID)

	outcomes, err := orchestrator.RecoverLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("RecoverLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want one failed outcome for %q", outcomes, req.ID)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed {
		t.Fatalf("Status = %q, want failed (never finished — §4.4: releasing on failure is not a successful judgment)", got.Status)
	}
	// Retry-able: RetryCardRequest must accept it from here.
	if err := orchestrator.RetryCardRequest(d.Conn, req.ID); err != nil {
		t.Fatalf("RetryCardRequest after recovery-failure: %v", err)
	}
}

func TestForceReleaseCardRequest_ReleasesRegardlessOfContinuationState(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	liveTask := newTestExecutionTask(t, d, "task-stuck", "proj-1", orchestrator.TaskStatusExecuting)
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, liveTask)

	if err := orchestrator.ForceReleaseCardRequest(d.Conn, req.ID, "operator says stuck"); err != nil {
		t.Fatalf("ForceReleaseCardRequest: %v", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed || got.Error != "operator says stuck" {
		t.Fatalf("got = %+v, want failed with the operator's reason", got)
	}
	if n, cerr := orchestrator.CountActiveCardRequests(d.Conn, cardID); cerr != nil || n != 0 {
		t.Fatalf("CountActiveCardRequests after force release = (%d, %v), want (0, nil)", n, cerr)
	}
}

func TestForceReleaseCardRequest_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	err := orchestrator.ForceReleaseCardRequest(d.Conn, "does-not-exist", "reason")
	if !errors.Is(err, orchestrator.ErrCardRequestNotFound) {
		t.Fatalf("ForceReleaseCardRequest(missing id) = %v, want ErrCardRequestNotFound", err)
	}
}
