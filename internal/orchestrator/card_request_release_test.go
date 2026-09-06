package orchestrator_test

// card_requests の枠解放・復旧走査 (card_request_release.go) のテスト。

import (
	"errors"
	"strings"
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
// card_request_release.go's own doc comment on that constraint). role should
// be "hook"/"exec" for a launcher's own job or "session" for a continuation
// — RecoverLaunchingCardRequests' reverse lookup only ever matches "session".
func insertTestJob(t *testing.T, d *db.DB, id, projectID, role, status, cardRequestID string) {
	t.Helper()
	insertTestJobAt(t, d, id, projectID, role, status, cardRequestID, time.Now().UTC())
}

// insertTestJobAt is insertTestJob with an explicit created_at, for tests
// that need to control job ordering (e.g. a stale prior attempt's job that
// must predate the current launching promotion).
func insertTestJobAt(t *testing.T, d *db.DB, id, projectID, role, status, cardRequestID string, createdAt time.Time) {
	t.Helper()
	if _, err := d.Conn.Exec(
		`INSERT INTO jobs (id, project_id, handler_id, role, status, card_request_id, created_at, updated_at) VALUES (?, ?, '', ?, ?, ?, ?, ?)`,
		id, projectID, role, status, cardRequestID, createdAt, createdAt,
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
// that only the job named by target_id (the session's own job) releases the
// slot — an unrelated terminal hook job for the same card does nothing.
func TestReconcileCardRequestSlots_SessionReleasesOnJobTerminalNotHookJob(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	insertTestJob(t, d, "hook-job-1", "proj-1", "hook", "completed", "")
	insertTestJob(t, d, "session-job-1", "proj-1", "session", "running", "")
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

	insertTestJob(t, d, "session-job-2", "proj-1", "session", "failed", "")
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
	insertTestJob(t, d, "launcher-crashed", "proj-1", "hook", "failed", req.ID)
	insertTestJob(t, d, "session-job-3", "proj-1", "session", "running", req.ID)

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
	insertTestJob(t, d, "launcher-crashed-2", "proj-1", "hook", "failed", req.ID)

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
		t.Fatalf("Status = %q, want failed (never finished — releasing on failure is not a successful judgment)", got.Status)
	}
	// Retry-able: RetryCardRequest must accept it from here.
	if err := orchestrator.RetryCardRequest(d.Conn, req.ID); err != nil {
		t.Fatalf("RetryCardRequest after recovery-failure: %v", err)
	}
}

// TestRecoverLaunchingCardRequests_ExcludesNonSessionRoleJobs pins that the
// reverse-lookup query's `role = 'session'` clause is load-bearing on its
// own: a job carrying the request's card_request_id but a non-session role
// (e.g. the launcher's OWN readonly exec job, which also gets
// card_request_id stamped so `boid card context` can read it) must NEVER be
// mistaken for the continuation.
func TestRecoverLaunchingCardRequests_ExcludesNonSessionRoleJobs(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-x"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	// Only candidate row: same card_request_id, but role=exec (not session).
	insertTestJob(t, d, "not-a-session-job", "proj-1", "exec", "completed", req.ID)

	outcomes, err := orchestrator.RecoverLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("RecoverLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want one FAILED outcome — a non-session-role job must never be mistaken for the continuation", outcomes)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.TargetID == "not-a-session-job" {
		t.Fatalf("got = %+v, must not have attached to the non-session-role job", got)
	}
}

// TestRecoverLaunchingCardRequests_ExcludesTheLauncherJobItself pins that
// `id != launcher_job_id` is independently load-bearing, separate from the
// role filter above: even a job that happens to carry role='session' must
// never be matched against itself if it IS the launcher's own job id. This
// can't happen for a real trigger-run launcher (always role=exec/hook), but
// pins the query's own defensive clause directly rather than relying on
// "no real launcher is ever role=session" holding forever.
func TestRecoverLaunchingCardRequests_ExcludesTheLauncherJobItself(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-y"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	// The ONLY candidate row IS the launcher's own job id, adversarially
	// tagged role=session and terminal.
	insertTestJob(t, d, "launcher-y", "proj-1", "session", "completed", req.ID)

	outcomes, err := orchestrator.RecoverLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("RecoverLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want one FAILED outcome — the launcher's own job id must never be matched as its own continuation", outcomes)
	}
}

// TestRecoverLaunchingCardRequests_IgnoresStalePriorAttemptSessionJob pins a
// regression: a retried request keeps its id, so a PRIOR attempt's session
// job can still carry the same card_request_id. Without excluding the
// launcher's own job by role AND bounding by the current attempt's start
// time, the reverse lookup could reattach to that stale, unrelated session
// instead of correctly reporting "no continuation found" for the current
// attempt.
func TestRecoverLaunchingCardRequests_IgnoresStalePriorAttemptSessionJob(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	// Attempt 1's own launcher job AND the session it dispatched — both
	// predate the current attempt and must never be picked by a later
	// recovery scan.
	insertTestJobAt(t, d, "launcher-1", "proj-1", "hook", "failed", req.ID, past)
	insertTestJobAt(t, d, "session-job-attempt-1", "proj-1", "session", "completed", req.ID, past)

	if err := orchestrator.FailCardRequest(d.Conn, req.ID, "attempt 1 failed"); err != nil {
		t.Fatalf("FailCardRequest: %v", err)
	}
	if err := orchestrator.RetryCardRequest(d.Conn, req.ID); err != nil {
		t.Fatalf("RetryCardRequest: %v", err)
	}
	def := orchestrator.CardRequestDefinition{CommandKey: "review"}
	if _, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "launcher-2", def); err != nil {
		t.Fatalf("ClaimQueuedCardRequests (attempt 2): %v", err)
	}
	// launcher-2 (attempt 2) crashes before dispatching anything.

	outcomes, err := orchestrator.RecoverLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("RecoverLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want ONE failed outcome (attempt 2 has no continuation of its own — must not reattach attempt 1's stale session)", outcomes)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed || got.TargetID == "session-job-attempt-1" {
		t.Fatalf("got = %+v, must not be attached to attempt 1's session-job-attempt-1", got)
	}
}

// TestReconcileCardRequestSlots_DeletedTaskReleasesAsFailed pins that a
// task-kind continuation deleted out from under an attached request (e.g.
// `boid task delete`) is treated as a terminal non-success, not "still
// live" — a deleted task can never report back, so leaving the slot
// attached forever would be the exact stuck-slot failure this function
// exists to prevent.
func TestReconcileCardRequestSlots_DeletedTaskReleasesAsFailed(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	task := newTestExecutionTask(t, d, "task-to-delete", "proj-1", orchestrator.TaskStatusExecuting)
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, task)
	if err := orchestrator.DeleteTask(d.Conn, task); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	outcomes, err := orchestrator.ReconcileCardRequestSlots(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileCardRequestSlots: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want one failed outcome for %q", outcomes, req.ID)
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

// TestForceReleaseCardRequest_DoesNotRequeueFoldedSiblings pins the
// force-release/abort asymmetry: an operator force-releasing a stuck slot
// means "stop this, don't restart it" — unlike FailCardRequest's default
// requeue behavior (TestFailCardRequest_ReleasesFoldedRequestsBackToQueued),
// a sibling folded into the force-released row must NOT come back to
// queued, or the very next claim would re-launch the card the operator just
// stopped. It goes to failed instead, same as the primary.
func TestForceReleaseCardRequest_DoesNotRequeueFoldedSiblings(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	first := &orchestrator.CardRequest{CardID: cardID}
	second := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := orchestrator.CreateCardRequest(d.Conn, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	primary, folded, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "launcher-1", orchestrator.CardRequestDefinition{})
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests: %v", err)
	}
	if len(folded) != 1 {
		t.Fatalf("folded = %d, want 1", len(folded))
	}

	if err := orchestrator.ForceReleaseCardRequest(d.Conn, primary.ID, "operator says stuck"); err != nil {
		t.Fatalf("ForceReleaseCardRequest: %v", err)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, second.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(second): %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("second.Status = %q, want failed — force-release must not requeue a folded sibling", got.Status)
	}
	if got.FoldedInto != "" {
		t.Errorf("second.FoldedInto = %q, want cleared", got.FoldedInto)
	}
	if got.Error == "operator says stuck" || !strings.Contains(got.Error, primary.ID) {
		t.Errorf("second.Error = %q, want text naming %q as what was force-released, not the verbatim reason (a reader must not mistake this sibling for the row the operator actually meant)", got.Error, primary.ID)
	}

	primaryGot, err := orchestrator.GetCardRequest(d.Conn, primary.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(primary): %v", err)
	}
	if primaryGot.Status != orchestrator.CardRequestStatusFailed || primaryGot.Error != "operator says stuck" {
		t.Errorf("primary = %+v, want status=failed with the operator's reason", primaryGot)
	}

	// The slot is free, but nothing is queued to restart the card with.
	if _, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "launcher-2", orchestrator.CardRequestDefinition{}); !errors.Is(err, orchestrator.ErrNoQueuedCardRequests) {
		t.Fatalf("ClaimQueuedCardRequests after force-release = %v, want ErrNoQueuedCardRequests", err)
	}
}

// ---- ReconcileLaunchingCardRequests: the PERIODIC self-heal for a `run:`
// script that exits/hangs without ever calling `boid task create` / `boid
// agent start`. Gated on the LAUNCHER JOB's own terminal status (never
// elapsed time) — see the function's own doc comment for why.

func TestReconcileLaunchingCardRequests_LauncherStillRunning_LeftAlone(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	insertTestJob(t, d, "launcher-1", "proj-1", "exec", "running", req.ID)

	outcomes, err := orchestrator.ReconcileLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want none (launcher job still running)", outcomes)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("Status = %q, want still launching", got.Status)
	}
}

// TestReconcileLaunchingCardRequests_LauncherTerminatedNoContinuation_FailsRetryable
// pins the core self-heal case: a `run:` script that exits without ever
// calling `boid task create` / `boid agent start` must fail (retry-able),
// not stay launching forever.
func TestReconcileLaunchingCardRequests_LauncherTerminatedNoContinuation_FailsRetryable(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	insertTestJob(t, d, "launcher-1", "proj-1", "exec", "completed", req.ID)

	outcomes, err := orchestrator.ReconcileLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want one failed outcome for %q", outcomes, req.ID)
	}
	if n, cerr := orchestrator.CountActiveCardRequests(d.Conn, cardID); cerr != nil || n != 0 {
		t.Fatalf("CountActiveCardRequests after self-heal = (%d, %v), want (0, nil) — the slot must be free again", n, cerr)
	}
}

// TestReconcileLaunchingCardRequests_MissingJobRowWithinGrace_LeftAlone pins
// that a launching row with no jobs row for its LauncherJobID YET (the gap
// between CreateCardRequest's commit and StartExec's own jobs INSERT) is
// left alone, not immediately failed — a reconcile tick can legitimately
// land in that gap.
func TestReconcileLaunchingCardRequests_MissingJobRowWithinGrace_LeftAlone(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-not-yet-created"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	// Deliberately no jobs row for "launcher-not-yet-created" — req.UpdatedAt
	// defaults to now, well within the grace window.

	outcomes, err := orchestrator.ReconcileLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want none (still within the job-row creation grace)", outcomes)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusLaunching {
		t.Fatalf("Status = %q, want still launching", got.Status)
	}
}

// TestReconcileLaunchingCardRequests_MissingJobRowPastGrace_FailsRetryable
// pins the other half: once the grace window has elapsed with still no
// jobs row at all, the launcher is gone (StartExec itself failed before
// ever reaching Dispatch's jobs INSERT — RunCardCommandAsHuman's own synchronous
// FailCardRequest on that path should normally have already caught this,
// but the self-heal must not depend on that call having succeeded).
func TestReconcileLaunchingCardRequests_MissingJobRowPastGrace_FailsRetryable(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-never-created"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	old := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := d.Conn.Exec(`UPDATE card_requests SET updated_at = ? WHERE id = ?`, old, req.ID); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}

	outcomes, err := orchestrator.ReconcileLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want one failed outcome for %q", outcomes, req.ID)
	}
}

// TestReconcileLaunchingCardRequests_LauncherTerminatedWithSessionContinuation_Attaches
// covers the crash-window case even outside a daemon restart: the launcher
// job (a readonly exec job) called `boid agent start`, which created the
// session job and returned, but the launcher's OWN process then died before
// its exec job settled to a terminal status in a way this scan can see
// (or, simply, the exec job legitimately finished right after dispatching a
// session and the periodic tick ran before the op's own attach — same
// reverse-lookup RecoverLaunchingCardRequests already relies on at startup).
func TestReconcileLaunchingCardRequests_LauncherTerminatedWithSessionContinuation_Attaches(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	insertTestJob(t, d, "launcher-1", "proj-1", "exec", "completed", req.ID)
	insertTestJob(t, d, "session-job-1", "proj-1", "session", "running", req.ID)

	outcomes, err := orchestrator.ReconcileLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusAttached) {
		t.Fatalf("outcomes = %+v, want one attached outcome for %q", outcomes, req.ID)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.TargetKind != orchestrator.CardRequestTargetKindSession || got.TargetID != "session-job-1" {
		t.Fatalf("got = %+v, want attached to session-job-1", got)
	}
}

// TestReconcileLaunchingCardRequests_SkipsGoCommandKeyRows pins that a Go
// reservation (CardRequestCommandKeyGo, acceptGo/workflow_card.go) is never
// touched by this periodic scan — Go has no real launcher job (its
// LauncherJobID is a synthetic marker, so a naive check would see "no such
// job" and immediately treat it as terminal), and would otherwise be failed
// out from under a legitimately in-flight Go call between its own
// CreateCardRequest and CreateTaskLinkedToCardRequest. No jobs row is
// inserted for the LauncherJobID at all, matching a real Go reservation.
func TestReconcileLaunchingCardRequests_SkipsGoCommandKeyRows(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{
		CardID: cardID, CommandKey: orchestrator.CardRequestCommandKeyGo,
		Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "go:synthetic-marker",
	}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	// Deliberately no jobs row for "go:synthetic-marker".

	outcomes, err := orchestrator.ReconcileLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want none (CardRequestCommandKeyGo rows must be skipped)", outcomes)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusLaunching {
		t.Fatalf("Status = %q, want still launching (untouched)", got.Status)
	}
}

// TestReconcileLaunchingCardRequests_EmptyCommandKeyRow_NotSkipped pins the
// second leg of the "no backfill needed for pre-__go__-sentinel rows"
// argument: only CardRequestCommandKeyGo ("__go__") is the skip predicate
// (TestReconcileLaunchingCardRequests_SkipsGoCommandKeyRows) — a row with an
// EMPTY command_key (the vocabulary a Go reservation used before that
// sentinel existed) is a real command-launcher row, not a Go reservation,
// and must still be reconciled normally like any other non-Go row.
func TestReconcileLaunchingCardRequests_EmptyCommandKeyRow_NotSkipped(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	insertTestJob(t, d, "launcher-1", "proj-1", "exec", "completed", req.ID)

	outcomes, err := orchestrator.ReconcileLaunchingCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("ReconcileLaunchingCardRequests: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].RequestID != req.ID || outcomes[0].Status != string(orchestrator.CardRequestStatusFailed) {
		t.Fatalf("outcomes = %+v, want one failed outcome for %q — an empty command_key must not be skipped like CardRequestCommandKeyGo", outcomes, req.ID)
	}
}

// ---- ReleaseCardRequestForTerminalTarget: the immediate, per-target
// release finalizeTerminal (internal/api) calls instead of waiting for
// ReconcileCardRequestSlots' own periodic tick.

func TestReleaseCardRequestForTerminalTarget_Success_Finishes(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	task := newTestExecutionTask(t, d, "task-1", "proj-1", orchestrator.TaskStatusDone)
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, task)

	found, err := orchestrator.ReleaseCardRequestForTerminalTarget(d.Conn, orchestrator.CardRequestTargetKindTask, task, true)
	if err != nil {
		t.Fatalf("ReleaseCardRequestForTerminalTarget: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFinished {
		t.Errorf("Status = %q, want finished", got.Status)
	}
}

func TestReleaseCardRequestForTerminalTarget_Failure_Fails(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	task := newTestExecutionTask(t, d, "task-1", "proj-1", orchestrator.TaskStatusAborted)
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, task)

	found, err := orchestrator.ReleaseCardRequestForTerminalTarget(d.Conn, orchestrator.CardRequestTargetKindTask, task, false)
	if err != nil {
		t.Fatalf("ReleaseCardRequestForTerminalTarget: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
}

// TestReleaseCardRequestForTerminalTarget_Failure_RequeuesFoldedSiblings pins
// the other half of the force-release/abort asymmetry
// (TestForceReleaseCardRequest_DoesNotRequeueFoldedSiblings is the other):
// a task ABORTING (an automatic, un-chosen outcome — not an operator's
// stop-this intent) goes through FailCardRequest's default requeue path, so
// a folded sibling comes back to queued and the next claim may restart the
// card. This is deliberately different from force-release.
func TestReleaseCardRequestForTerminalTarget_Failure_RequeuesFoldedSiblings(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	first := &orchestrator.CardRequest{CardID: cardID}
	second := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := orchestrator.CreateCardRequest(d.Conn, second); err != nil {
		t.Fatalf("create second: %v", err)
	}
	primary, folded, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "launcher-1", orchestrator.CardRequestDefinition{})
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests: %v", err)
	}
	if len(folded) != 1 {
		t.Fatalf("folded = %d, want 1", len(folded))
	}
	task := newTestExecutionTask(t, d, "task-1", "proj-1", orchestrator.TaskStatusAborted)
	if err := orchestrator.AttachCardRequest(d.Conn, primary.ID, orchestrator.CardRequestTargetKindTask, task); err != nil {
		t.Fatalf("attach: %v", err)
	}

	found, err := orchestrator.ReleaseCardRequestForTerminalTarget(d.Conn, orchestrator.CardRequestTargetKindTask, task, false)
	if err != nil {
		t.Fatalf("ReleaseCardRequestForTerminalTarget: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}

	got, err := orchestrator.GetCardRequest(d.Conn, second.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(second): %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusQueued || got.FoldedInto != "" {
		t.Errorf("second = %+v, want status=queued folded_into=\"\" — an abort must requeue a folded sibling (unlike force-release)", got)
	}
}

func TestReleaseCardRequestForTerminalTarget_NoAttachedRow_ReturnsNotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	found, err := orchestrator.ReleaseCardRequestForTerminalTarget(d.Conn, orchestrator.CardRequestTargetKindTask, "no-such-task", true)
	if err != nil {
		t.Fatalf("ReleaseCardRequestForTerminalTarget: %v", err)
	}
	if found {
		t.Fatal("found = true, want false")
	}
}

func TestTaskRepository_ReleaseCardRequestForTerminalTarget_WrapsInOwnTx(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	task := newTestExecutionTask(t, d, "task-1", "proj-1", orchestrator.TaskStatusDone)
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, task)

	repo := orchestrator.NewTaskRepository(d.Conn)
	found, err := repo.ReleaseCardRequestForTerminalTarget(orchestrator.CardRequestTargetKindTask, task, true)
	if err != nil {
		t.Fatalf("ReleaseCardRequestForTerminalTarget: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFinished {
		t.Errorf("Status = %q, want finished", got.Status)
	}
}

func TestForceReleaseCardRequest_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	err := orchestrator.ForceReleaseCardRequest(d.Conn, "does-not-exist", "reason")
	if !errors.Is(err, orchestrator.ErrCardRequestNotFound) {
		t.Fatalf("ForceReleaseCardRequest(missing id) = %v, want ErrCardRequestNotFound", err)
	}
}
