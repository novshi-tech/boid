package orchestrator_test

// Tests IngestCardEventRequest and its CreateAction wiring.

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// seedProject inserts a projects row (and, unless workspaceID is "", a
// project_workspaces row linking it).
func seedProject(t *testing.T, dbtx db.DBTX, projectID, workspaceID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := dbtx.Exec(
		`INSERT INTO projects (id, work_dir, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		projectID, "/tmp/"+projectID, now, now,
	); err != nil {
		t.Fatalf("insert project %q: %v", projectID, err)
	}
	if workspaceID == "" {
		return
	}
	if _, err := dbtx.Exec(
		`INSERT INTO project_workspaces (project_id, workspace_id) VALUES (?, ?)`,
		projectID, workspaceID,
	); err != nil {
		t.Fatalf("insert project_workspaces %q/%q: %v", projectID, workspaceID, err)
	}
}

// seedCardTask inserts a working type='card' task owned by projectID.
func seedCardTask(t *testing.T, dbtx db.DBTX, taskID, projectID string) {
	t.Helper()
	seedCardTaskWithStatus(t, dbtx, taskID, projectID, orchestrator.TaskStatusWorking)
}

// seedExecutionTask inserts a type='execution' task owned by projectID.
func seedExecutionTask(t *testing.T, dbtx db.DBTX, taskID, projectID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := dbtx.Exec(
		`INSERT INTO tasks (id, type, project_id, title, status, behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start, created_at, updated_at)
		 VALUES (?, 'execution', ?, ?, 'executing', '', '[]', FALSE, '', '', '{}', '[]', FALSE, ?, ?)`,
		taskID, projectID, "exec "+taskID, now, now,
	); err != nil {
		t.Fatalf("insert execution task %q: %v", taskID, err)
	}
}

func newAction(id, taskID, actionType, actor string) *orchestrator.Action {
	return &orchestrator.Action{
		ID:        id,
		TaskID:    taskID,
		Type:      actionType,
		Actor:     actor,
		CreatedAt: time.Now().UTC(),
	}
}

// stubCardEventResolver is a CardEventResolver test double: project id ->
// card_events.command key.
type stubCardEventResolver map[string]string

func (r stubCardEventResolver) CardEventCommand(projectID string) (string, bool) {
	key, ok := r[projectID]
	if !ok || key == "" {
		return "", false
	}
	return key, true
}

func seedCardTaskWithStatus(t *testing.T, dbtx db.DBTX, taskID, projectID string, status orchestrator.TaskStatus) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := dbtx.Exec(
		`INSERT INTO tasks (id, type, project_id, title, status, kind, urgency, wake_task_id, suggestion_verb, detail, created_at, updated_at)
		 VALUES (?, 'card', ?, ?, ?, '', '', '', '', '{}', ?, ?)`,
		taskID, projectID, "card "+taskID, string(status), now, now,
	); err != nil {
		t.Fatalf("insert card task %q: %v", taskID, err)
	}
}

func listCardRequests(t *testing.T, dbtx db.DBTX, cardID string) []*orchestrator.CardRequest {
	t.Helper()
	rows, err := orchestrator.ListCardRequestsByCard(dbtx, cardID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	return rows
}

// --- eligibility gates ---

func TestIngestCardEventRequest_NilResolver_NoOp(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusParked)
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorHuman)

	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, nil); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	if got := listCardRequests(t, d.Conn, "card-1"); len(got) != 0 {
		t.Fatalf("got %d card_requests, want 0 (nil resolver)", len(got))
	}
}

func TestIngestCardEventRequest_ProjectDeclaresNoCardEvents_NoOp(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusParked)
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorHuman)

	resolver := stubCardEventResolver{} // proj-a declares nothing
	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	if got := listCardRequests(t, d.Conn, "card-1"); len(got) != 0 {
		t.Fatalf("got %d card_requests, want 0 (no card_events.command declared)", len(got))
	}
}

// An execution task cannot hold parked/working under the tasks CHECK
// constraints, so the status guard rejects this row before the task-type
// guard is consulted — removing the type guard leaves this test green.
func TestIngestCardEventRequest_TaskNotCard_NoOp(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedExecutionTask(t, d.Conn, "exec-1", "proj-a")
	a := newAction("act-1", "exec-1", "attrs_set", orchestrator.ActorHuman)

	resolver := stubCardEventResolver{"proj-a": "review"}
	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	if got := listCardRequests(t, d.Conn, "exec-1"); len(got) != 0 {
		t.Fatalf("got %d card_requests, want 0 (target is not a card task)", len(got))
	}
}

func TestIngestCardEventRequest_UnknownTask_NoOp(t *testing.T) {
	d := testutil.NewTestDB(t)
	a := newAction("act-1", "no-such-task", "attrs_set", orchestrator.ActorHuman)
	resolver := stubCardEventResolver{"proj-a": "review"}
	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
}

// --- card status guard: parked/working only ---

func TestIngestCardEventRequest_CardStatusGuard(t *testing.T) {
	cases := []struct {
		status      orchestrator.TaskStatus
		wantCreated bool
	}{
		{orchestrator.TaskStatusParked, true},
		{orchestrator.TaskStatusWorking, true},
		{orchestrator.TaskStatusDone, false},
		{orchestrator.TaskStatusDropped, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			d := testutil.NewTestDB(t)
			seedProject(t, d.Conn, "proj-a", "")
			seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", tc.status)
			a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorHuman)
			resolver := stubCardEventResolver{"proj-a": "review"}

			if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
				t.Fatalf("IngestCardEventRequest: %v", err)
			}
			got := listCardRequests(t, d.Conn, "card-1")
			if tc.wantCreated && len(got) != 1 {
				t.Fatalf("status %q: got %d card_requests, want 1", tc.status, len(got))
			}
			if !tc.wantCreated && len(got) != 0 {
				t.Fatalf("status %q: got %d card_requests, want 0", tc.status, len(got))
			}
		})
	}
}

// --- action type allowlist: the full card-machine vocabulary, one entry
// per type, pinned individually so flipping any single map entry in the
// allowlist breaks exactly the case for that type. ---

func TestIngestCardEventRequest_ActionTypeAllowlist(t *testing.T) {
	allowed := map[string]bool{
		// New information arrived from outside the card's own bookkeeping.
		"child_closed": true,
		"wake_due":     true,
		"noted":        true,
		"attrs_set":    true,
		// The daemon's self-recorded state changes.
		orchestrator.ActionTypeCardCreated:      true,
		orchestrator.ActionTypeCardEdited:       true,
		orchestrator.ActionTypeIdentityLinked:   true,
		orchestrator.ActionTypeIdentityUnlinked: true,
	}
	excluded := []string{
		"go", "start", "park", "complete", "drop", "reopen",
		"child_added", "child_specced", "child_dropped",
		"progress", "child_dispatched", "done_request", "fail_request",
		// A human's answer to a suggestion — accept executes a decision the
		// card already carries, reject withdraws it. Neither adds material
		// the next decision could read.
		"answered",
		// FinishCardRequest/FailCardRequest's own self-record: a card
		// command's continuation terminating must not itself start a new one.
		orchestrator.ActionTypeCommandFinished, orchestrator.ActionTypeCommandFailed, orchestrator.ActionTypeCommandForceReleased,
	}
	all := map[string]bool{}
	for k := range allowed {
		all[k] = true
	}
	for _, k := range excluded {
		all[k] = false
	}

	for actionType, wantCreated := range all {
		t.Run(actionType, func(t *testing.T) {
			d := testutil.NewTestDB(t)
			seedProject(t, d.Conn, "proj-a", "")
			seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusParked)
			a := newAction("act-1", "card-1", actionType, orchestrator.ActorHuman)
			resolver := stubCardEventResolver{"proj-a": "review"}

			if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
				t.Fatalf("IngestCardEventRequest: %v", err)
			}
			got := listCardRequests(t, d.Conn, "card-1")
			if wantCreated && len(got) != 1 {
				t.Fatalf("action %q: got %d card_requests, want 1", actionType, len(got))
			}
			if !wantCreated && len(got) != 0 {
				t.Fatalf("action %q: got %d card_requests, want 0", actionType, len(got))
			}
		})
	}
}

// --- queued row content ---

func TestIngestCardEventRequest_CreatesQueuedRow_ContentPinned(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusParked)
	a := newAction("act-1", "card-1", "child_closed", orchestrator.ActorDaemon)
	resolver := stubCardEventResolver{"proj-a": "review"}

	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 1 {
		t.Fatalf("got %d card_requests, want 1", len(got))
	}
	row := got[0]
	if row.CardID != "card-1" {
		t.Errorf("CardID = %q, want card-1", row.CardID)
	}
	if row.CommandKey != "review" {
		t.Errorf("CommandKey = %q, want review", row.CommandKey)
	}
	if row.CauseID != "act-1" {
		t.Errorf("CauseID = %q, want act-1 (the causing action's id)", row.CauseID)
	}
	if row.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("Status = %q, want queued", row.Status)
	}
	if row.Launched != (orchestrator.CardRequestDefinition{}) {
		t.Errorf("Launched = %+v, want zero value (snapshot happens at queued->launching, not here)", row.Launched)
	}
}

// --- occupied slot: queued rows accumulate even while the card's slot is
// already launching/attached ---

func TestIngestCardEventRequest_SlotOccupied_StillQueues(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusWorking)
	if err := orchestrator.CreateCardRequest(d.Conn, &orchestrator.CardRequest{
		CardID: "card-1", CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-launcher",
	}); err != nil {
		t.Fatalf("seed occupying card request: %v", err)
	}

	a := newAction("act-1", "card-1", "wake_due", orchestrator.ActorDaemon)
	resolver := stubCardEventResolver{"proj-a": "review"}
	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 2 {
		t.Fatalf("got %d card_requests, want 2 (occupying + newly queued)", len(got))
	}
}

// --- redelivery: same cause_id is a no-op, not an error ---

func TestIngestCardEventRequest_DuplicateCause_NoOpNotError(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusParked)
	a := newAction("act-1", "card-1", "noted", orchestrator.ActorDaemon)
	resolver := stubCardEventResolver{"proj-a": "review"}

	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	// Redelivery of the exact same action (same id).
	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("redelivered ingest must be a quiet no-op, got error: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 1 {
		t.Fatalf("got %d card_requests, want 1 (redelivery must not create a second row)", len(got))
	}
}

// --- self-loop exclusion by launch origin, not project/actor ---

func withWriterCardRequestID(ctx context.Context, id string) context.Context {
	return orchestrator.WithWriterCardRequestID(ctx, id)
}

func TestIngestCardEventRequest_SelfLoop_WriterOwnsLiveRequestForSameCard_NoOp(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusWorking)
	req := &orchestrator.CardRequest{CardID: "card-1", CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("seed live request: %v", err)
	}

	ctx := withWriterCardRequestID(context.Background(), req.ID)
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorTask("t-1"))
	resolver := stubCardEventResolver{"proj-a": "review"}
	if err := orchestrator.IngestCardEventRequest(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 1 {
		t.Fatalf("got %d card_requests, want 1 (only the seeded launching row — self-loop must not queue a second)", len(got))
	}
}

func TestIngestCardEventRequest_WriterRequestForDifferentCard_NotSelfLoop_Creates(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusWorking)
	seedCardTaskWithStatus(t, d.Conn, "card-2", "proj-a", orchestrator.TaskStatusWorking)
	// Writer holds the live slot for card-2, but this action targets card-1.
	req := &orchestrator.CardRequest{CardID: "card-2", CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("seed live request: %v", err)
	}

	ctx := withWriterCardRequestID(context.Background(), req.ID)
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorTask("t-1"))
	resolver := stubCardEventResolver{"proj-a": "review"}
	if err := orchestrator.IngestCardEventRequest(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 1 {
		t.Fatalf("got %d card_requests, want 1 (different card, not a self-loop)", len(got))
	}
}

func TestIngestCardEventRequest_WriterRequestFinished_NotSelfLoop_Creates(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusWorking)
	req := &orchestrator.CardRequest{CardID: "card-1", CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("seed request: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "task-1"); err != nil {
		t.Fatalf("attach request: %v", err)
	}
	if err := orchestrator.FinishCardRequest(d.Conn, req.ID, "done"); err != nil {
		t.Fatalf("finish request: %v", err)
	}

	// The writer's own request has already terminated (released the slot);
	// a new event from the same origin id is not a self-loop.
	ctx := withWriterCardRequestID(context.Background(), req.ID)
	a := newAction("act-1", "card-1", "attrs_set", orchestrator.ActorTask("t-1"))
	resolver := stubCardEventResolver{"proj-a": "review"}
	if err := orchestrator.IngestCardEventRequest(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 2 {
		t.Fatalf("got %d card_requests, want 2 (the finished request + a newly queued one — finished no longer holds the slot, not a self-loop)", len(got))
	}
}

func TestIngestCardEventRequest_NoWriterContext_NotSelfLoop_Creates(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusParked)
	a := newAction("act-1", "card-1", "noted", orchestrator.ActorHuman)
	resolver := stubCardEventResolver{"proj-a": "review"}

	// No WithWriterCardRequestID at all — e.g. a daemon-recorded
	// child_closed, or a human's Web UI/CLI write.
	if err := orchestrator.IngestCardEventRequest(context.Background(), d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 1 {
		t.Fatalf("got %d card_requests, want 1", len(got))
	}
}

func TestIngestCardEventRequest_WriterCardRequestIDEmpty_NotSelfLoop_Creates(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusParked)
	a := newAction("act-1", "card-1", "noted", orchestrator.ActorHuman)
	resolver := stubCardEventResolver{"proj-a": "review"}

	// A writer context exists (this write came through ExecuteBoidBuiltin)
	// but carries no card request id — e.g. a plain sandbox write that is
	// not a card-command continuation at all.
	ctx := withWriterCardRequestID(context.Background(), "")
	if err := orchestrator.IngestCardEventRequest(ctx, d.Conn, a, resolver); err != nil {
		t.Fatalf("IngestCardEventRequest: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 1 {
		t.Fatalf("got %d card_requests, want 1", len(got))
	}
}

// --- CreateAction integration: same-transaction wiring, tx-failing errors ---

func TestCreateAction_ChildClosed_CreatesQueuedCardRequest_SameTx(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusWorking)

	repo := orchestrator.NewTaskRepository(d.Conn)
	repo.SetCardEventResolver(stubCardEventResolver{"proj-a": "review"})

	a := &orchestrator.Action{TaskID: "card-1", Type: "child_closed", Actor: orchestrator.ActorDaemon}
	if err := repo.CreateAction(context.Background(), a); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	got := listCardRequests(t, d.Conn, "card-1")
	if len(got) != 1 {
		t.Fatalf("got %d card_requests, want 1", len(got))
	}
	if got[0].CauseID != a.ID {
		t.Errorf("CauseID = %q, want the action id %q", got[0].CauseID, a.ID)
	}
}

func TestCreateAction_CardEventIngestExcludedAction_NoCardRequest(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusWorking)

	repo := orchestrator.NewTaskRepository(d.Conn)
	repo.SetCardEventResolver(stubCardEventResolver{"proj-a": "review"})

	a := &orchestrator.Action{TaskID: "card-1", Type: "progress", Actor: orchestrator.ActorDaemon}
	if err := repo.CreateAction(context.Background(), a); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if got := listCardRequests(t, d.Conn, "card-1"); len(got) != 0 {
		t.Fatalf("got %d card_requests, want 0 (progress is excluded)", len(got))
	}
}

// TestCreateAction_CardEventIngestHardError_RollsBackActionToo pins the
// tx-failing error policy: a genuine card-event ingest error must fail
// CreateAction's whole transaction — the action row must not persist
// either. Forces a real
// error (not ErrCardRequestDuplicateCause) by dropping the card_requests
// table out from under a legitimately eligible ingest.
func TestCreateAction_CardEventIngestHardError_RollsBackActionToo(t *testing.T) {
	d := testutil.NewTestDB(t)
	seedProject(t, d.Conn, "proj-a", "")
	seedCardTaskWithStatus(t, d.Conn, "card-1", "proj-a", orchestrator.TaskStatusWorking)

	repo := orchestrator.NewTaskRepository(d.Conn)
	repo.SetCardEventResolver(stubCardEventResolver{"proj-a": "review"})

	if _, err := d.Conn.Exec(`DROP TABLE card_requests`); err != nil {
		t.Fatalf("drop card_requests: %v", err)
	}

	a := &orchestrator.Action{TaskID: "card-1", Type: "child_closed", Actor: orchestrator.ActorDaemon}
	if err := repo.CreateAction(context.Background(), a); err == nil {
		t.Fatal("expected CreateAction to fail when the card-event ingest step hits a real DB error")
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, "card-1")
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("got %d actions, want 0 (the action INSERT must roll back with the failed ingest)", len(actions))
	}
}
