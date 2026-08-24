package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type recordingTxStore struct {
	task             *orchestrator.Task
	updatedTask      *orchestrator.Task
	actions          []*orchestrator.Action
	triage           map[string]*orchestrator.TaskTriage
	parkedFromFn     func(taskID string) (orchestrator.TaskStatus, error)
	getTaskTriageErr error // when set, GetTaskTriage returns this instead of the usual not-found
	// tasks backs multi-task scenarios (e.g. recordChildClosedOnParent, which
	// looks up the PARENT task by id while the primary `task`/`updatedTask`
	// fields track the child under test). Checked before task/updatedTask.
	tasks map[string]*orchestrator.Task
	// unlinkAllForTaskCalls records every UnlinkAllForTask(taskID) call — used
	// by the ingestion-identity.md PR-1 drop-side-effect tests to pin that
	// `drop` (and ONLY drop) releases a task's identity bindings.
	unlinkAllForTaskCalls []string
}

func (s *recordingTxStore) CreateTask(task *orchestrator.Task) error { return nil }
func (s *recordingTxStore) GetTask(id string) (*orchestrator.Task, error) {
	// Prefer the most recently committed update (if any) over the original
	// snapshot, so a second WithinTx call within the same test (e.g. PR-2's
	// ApplyAction("ready") auto-chaining into Dispatch, which opens its own
	// separate transaction right after "ready"'s commits) observes the prior
	// transaction's write — mirroring production, where Tx and any later
	// GetTask both read the same underlying DB.
	if s.updatedTask != nil && s.updatedTask.ID == id {
		return s.updatedTask, nil
	}
	if s.tasks != nil {
		if t, ok := s.tasks[id]; ok {
			return t, nil
		}
	}
	if s.task == nil || s.task.ID != id {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return s.task, nil
}
func (s *recordingTxStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) UpdateTask(task *orchestrator.Task) error {
	s.updatedTask = task
	return nil
}
func (s *recordingTxStore) DeleteTask(id string) error { return nil }
func (s *recordingTxStore) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) FindTaskByRef(ref, parentID, projectID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) ListChildren(parentID string) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) CreateAction(action *orchestrator.Action) error {
	s.actions = append(s.actions, action)
	return nil
}

// ListActionsByTask filters s.actions (both pre-seeded fixture rows and any
// CreateAction'd during the test) by taskID — docs/plans/ingestion-identity.md
// PR-5 (B-6)'s autoReopen (triage_done.go) reads this inside its own Tx to
// derive the フラップ count (orchestrator.CountAutoReopens), so this stub
// returning an unconditional nil (its pre-PR-5 shape — no existing test
// asserted on its return value) would silently make every フラップ test pass
// for the wrong reason.
func (s *recordingTxStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	var out []*orchestrator.Action
	for _, a := range s.actions {
		if a.TaskID == taskID {
			out = append(out, a)
		}
	}
	return out, nil
}
func (s *recordingTxStore) SeedTaskTriage(taskID string) error {
	if s.triage == nil {
		s.triage = map[string]*orchestrator.TaskTriage{}
	}
	if _, ok := s.triage[taskID]; !ok {
		s.triage[taskID] = &orchestrator.TaskTriage{TaskID: taskID}
	}
	return nil
}

func (s *recordingTxStore) UpsertTaskTriage(tt *orchestrator.TaskTriage) error {
	if s.triage == nil {
		s.triage = map[string]*orchestrator.TaskTriage{}
	}
	s.triage[tt.TaskID] = tt
	return nil
}
func (s *recordingTxStore) GetTaskTriage(taskID string) (*orchestrator.TaskTriage, error) {
	if s.getTaskTriageErr != nil {
		return nil, s.getTaskTriageErr
	}
	tt, ok := s.triage[taskID]
	if !ok {
		return nil, fmt.Errorf("task_triage not found: %s: %w", taskID, sql.ErrNoRows)
	}
	return tt, nil
}
func (s *recordingTxStore) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*orchestrator.TaskTriage, error) {
	out := map[string]*orchestrator.TaskTriage{}
	for _, id := range taskIDs {
		if tt, ok := s.triage[id]; ok {
			out[id] = tt
		}
	}
	return out, nil
}
func (s *recordingTxStore) DeleteTaskTriage(taskID string) error {
	delete(s.triage, taskID)
	return nil
}
func (s *recordingTxStore) ParkedFrom(taskID string) (orchestrator.TaskStatus, error) {
	if s.parkedFromFn != nil {
		return s.parkedFromFn(taskID)
	}
	return "", fmt.Errorf("parked_from not found: %s", taskID)
}
func (s *recordingTxStore) GetJob(id string) (*Job, error) { return nil, fmt.Errorf("not implemented") }
func (s *recordingTxStore) ListJobsByTask(taskID string) ([]*Job, error) {
	return nil, nil
}
func (s *recordingTxStore) UpdateJob(job *Job) error { return nil }

// LinkIdentity / UnlinkIdentity / ResolveIdentity / ListIdentitiesByTask are
// not exercised by the drop-side-effect tests (only UnlinkAllForTask is
// called from within ApplyAction) — trivial no-ops, kept just to satisfy
// TxStore.
func (s *recordingTxStore) LinkIdentity(projectID, identity, taskID string) error { return nil }
func (s *recordingTxStore) UnlinkIdentity(projectID, identity string) error       { return nil }
func (s *recordingTxStore) UnlinkAllForTask(taskID string) error {
	s.unlinkAllForTaskCalls = append(s.unlinkAllForTaskCalls, taskID)
	return nil
}
func (s *recordingTxStore) ResolveIdentity(projectID, identity string) (*orchestrator.Task, error) {
	return nil, orchestrator.ErrTaskNotFound
}
func (s *recordingTxStore) ListIdentitiesByTask(taskID string) ([]string, error) {
	return nil, nil
}

type recordingTransactor struct {
	store *recordingTxStore
}

func (t recordingTransactor) WithinTx(fn func(TxStore) error) error {
	return fn(t.store)
}

type dispatchContextProbe struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func newDispatchContextProbe() *dispatchContextProbe {
	return &dispatchContextProbe{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (p *dispatchContextProbe) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}

	select {
	case <-ctx.Done():
		select {
		case <-p.canceled:
		default:
			close(p.canceled)
		}
		return nil, ctx.Err()
	case <-p.release:
		return &orchestrator.DispatchResult{FinalPayload: task.Payload}, nil
	}
}

func (p *dispatchContextProbe) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

func TestTaskWorkflowServiceApplyAction_BackgroundDispatchMustOutliveRequestContext(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Title:     "start task",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}

	txStore := &recordingTxStore{}
	probe := newDispatchContextProbe()
	defer close(probe.release)

	svc := &TaskWorkflowService{
		Tasks:       &stubTaskStore{task: task},
		Tx:          recordingTransactor{store: txStore},
		Meta:        stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		Coordinator: probe,
	}

	ctx, cancel := context.WithCancel(context.Background())
	result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want %q", result.Task.Status, orchestrator.TaskStatusExecuting)
	}

	select {
	case <-probe.started:
	case <-time.After(1 * time.Second):
		t.Fatal("dispatch loop did not start")
	}

	cancel()

	select {
	case <-probe.canceled:
		t.Fatal("background dispatch inherited request context cancellation")
	case <-time.After(100 * time.Millisecond):
	}
}

// ---- Pre-execution actions (cross-project-issue-triage Phase 1 PR-1) ----

// isPreExecutionCardStatus reports whether status is one of the five
// statuses that exist ONLY on the card side of the split (PR-B,
// machine_card.go's preExecutionStatuses) — captured/triaged/parked/ready/
// working. done/aborted/dropped (and every execution-lifecycle status) are
// deliberately excluded: those are either shared between both machines or
// execution-only, so "does this task have a sidecar row" is genuinely
// ambiguous/test-scenario-dependent there, unlike the five pre-execution
// statuses where production always seeds one at creation (see
// newTriageWorkflowService's own doc comment).
func isPreExecutionCardStatus(status orchestrator.TaskStatus) bool {
	switch status {
	case orchestrator.TaskStatusCaptured, orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked, orchestrator.TaskStatusReady, orchestrator.TaskStatusWorking:
		return true
	default:
		return false
	}
}

// newTriageWorkflowService builds a TaskWorkflowService for a card (triage)
// task under test. PR-B (docs/plans/suggestion-as-state-transition-impl.md
// §2): machineFor now picks NewCardMachine vs NewExecutionMachine by
// checking whether the task carries a task_triage sidecar row, so — for a
// task whose status is one of the five pre-execution-only statuses — this
// helper seeds an empty row for task.ID when the caller's txStore doesn't
// already carry one, and wires TaskTriage to see it. This mirrors
// production's own invariant (task_create.go's CreateTask /
// task_resolve_or_capture.go's ResolveOrCapture both seed an empty
// task_triage row up front for any pre-execution initial_status,
// unconditional on the caller ever sending a park/attrs_set). Without this,
// every test built on this helper for those five statuses would have
// machineFor fall back to NewExecutionMachine (no row = "not a card"), which
// has no rule at all for triage/ready/park/drop/etc. and would reject them
// with a spurious 400.
//
// Deliberately NOT extended to done/aborted/dropped/pending/executing/
// awaiting: several tests built on this helper (attrs_set_done_test.go's
// TaskTriageStoreNotWired/NoTaskTriageRow cases, triage_done_pr5_test.go's
// reopen-routing cases) construct a task in one of THOSE statuses
// specifically to exercise TaskTriage being nil, empty, or erroring — auto-
// wiring/seeding for them would silently defeat those tests' own premise.
// Each such test wires (or deliberately leaves unwired) TaskTriage itself,
// same as before this helper existed.
func newTriageWorkflowService(task *orchestrator.Task, txStore *recordingTxStore) *TaskWorkflowService {
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		// Coordinator intentionally nil: pre-execution transitions have no
		// hooks to dispatch (no runtime exists for a triage task).
	}
	if isPreExecutionCardStatus(task.Status) {
		if txStore.triage == nil {
			txStore.triage = map[string]*orchestrator.TaskTriage{}
		}
		if _, ok := txStore.triage[task.ID]; !ok {
			txStore.triage[task.ID] = &orchestrator.TaskTriage{TaskID: task.ID}
		}
		svc.TaskTriage = txStore
	}
	return svc
}

func TestTaskWorkflowServiceApplyAction_Triage_CapturedToTriaged(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusCaptured, Behavior: "dev", Payload: []byte(`{}`)}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})
	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "triage"})
	if err != nil {
		t.Fatalf("ApplyAction(triage): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("status = %q, want triaged", result.Task.Status)
	}
}

// TestTaskWorkflowServiceApplyAction_StampsActorFromContext verifies
// ApplyAction reads orchestrator.ActorFromContext(ctx) and stamps it onto the
// recorded Action — the propagation path 論点11「代行タスク」 depends on: the
// HTTP/Web/CLI human path wraps ctx with ActorHuman (action.go, web_service.go),
// the brokered sandbox path (boid_executor.go, task_notify.go) wraps it with
// ActorTask(callingTaskID). A caller that forgets to wrap ctx gets "" — no
// actor is silently fabricated.
func TestTaskWorkflowServiceApplyAction_StampsActorFromContext(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusCaptured, Behavior: "dev", Payload: []byte(`{}`)}

	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"human", orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), orchestrator.ActorHuman},
		{"task", orchestrator.WithActor(context.Background(), orchestrator.ActorTask("proxy-task-1")), orchestrator.ActorTask("proxy-task-1")},
		{"unset", context.Background(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			txStore := &recordingTxStore{task: task}
			svc := newTriageWorkflowService(task, txStore)
			if _, err := svc.ApplyAction(tc.ctx, task.ID, ApplyActionRequest{Type: "triage"}); err != nil {
				t.Fatalf("ApplyAction(triage): %v", err)
			}
			if len(txStore.actions) != 1 {
				t.Fatalf("expected 1 recorded action, got %d", len(txStore.actions))
			}
			if got := txStore.actions[0].Actor; got != tc.want {
				t.Fatalf("actor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTaskWorkflowServiceApplyAction_Park_UpsertsWakeCondition verifies the
// park-specific post-processing writes wake_at/wake_task_id into task_triage,
// preserving any existing kind/urgency/detail on the sidecar row (Opus指摘
// #1's "give park a real writer" resolution).
func TestTaskWorkflowServiceApplyAction_Park_UpsertsWakeCondition(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.TaskTriage{
			"t1": {TaskID: "t1", Kind: "issue", Urgency: "week", Detail: []byte(`{"summary":"keep me"}`)},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	payload := []byte(`{"wake_at":"2026-09-01T00:00:00Z","wake_task_id":"blocking-task"}`)
	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "park", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(park): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusParked {
		t.Fatalf("status = %q, want parked", result.Task.Status)
	}

	got := txStore.triage["t1"]
	if got == nil {
		t.Fatal("expected task_triage row to exist after park")
	}
	if got.Kind != "issue" || got.Urgency != "week" || string(got.Detail) != `{"summary":"keep me"}` {
		t.Fatalf("park must preserve existing kind/urgency/detail, got %+v", got)
	}
	wantWakeAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got.WakeAt == nil || !got.WakeAt.Equal(wantWakeAt) {
		t.Fatalf("WakeAt = %v, want %v", got.WakeAt, wantWakeAt)
	}
	if got.WakeTaskID != "blocking-task" {
		t.Fatalf("WakeTaskID = %q, want %q", got.WakeTaskID, "blocking-task")
	}
}

// park with no payload still creates a sidecar row (WakeAt/WakeTaskID empty)
// so ParkedFrom has a stable place to be read from later, even for a task
// that never had a task_triage row before (e.g. created via a path that
// predates this feature, or a test fixture).
func TestTaskWorkflowServiceApplyAction_Park_NoPayload_CreatesEmptySidecarRow(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "park"}); err != nil {
		t.Fatalf("ApplyAction(park): %v", err)
	}
	got := txStore.triage["t1"]
	if got == nil {
		t.Fatal("expected task_triage row to be created")
	}
	if got.WakeAt != nil {
		t.Fatalf("WakeAt = %v, want nil (no payload provided)", got.WakeAt)
	}
}

// TestTaskWorkflowServiceApplyAction_Park_PropagatesNonNotFoundTriageError is
// the regression test for codex review round 1's Minor: applyParkSideEffect
// used to treat every GetTaskTriage error (not just "no row") as "create a
// fresh sidecar", which on a transient DB error would silently blow away an
// existing row's kind/urgency/detail instead of surfacing the failure.
func TestTaskWorkflowServiceApplyAction_Park_PropagatesNonNotFoundTriageError(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task, getTaskTriageErr: fmt.Errorf("db connection reset")}
	svc := newTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "park"})
	if err == nil {
		t.Fatal("expected the transient GetTaskTriage error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "db connection reset") {
		t.Fatalf("error = %v, want it to mention the underlying transient failure", err)
	}
}

// TestTaskWorkflowService_Wake_NeverReadsOutsideTransaction pins down the
// codex review round 1 Major fix: Wake must resolve GetTask/ParkedFrom
// entirely from within the WithinTx closure, not from a pre-tx read of
// s.Tasks, so there is no window between reading the park origin and
// writing the resolved transition for a concurrent park/wake cycle to land
// in. Setting Tasks to nil proves Wake never touches it — if it did, this
// would panic on the nil interface.
func TestTaskWorkflowService_Wake_NeverReadsOutsideTransaction(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task:         task,
		parkedFromFn: func(taskID string) (orchestrator.TaskStatus, error) { return orchestrator.TaskStatusTriaged, nil },
	}
	svc := &TaskWorkflowService{
		Tasks: nil, // deliberately nil — Wake must never dereference this
		Tx:    recordingTransactor{store: txStore},
	}

	result, err := svc.Wake(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("status = %q, want triaged", result.Task.Status)
	}
}

func TestTaskWorkflowService_Wake_RoundTrip_TriagedThenReady(t *testing.T) {
	cases := []struct {
		name       string
		parkedFrom orchestrator.TaskStatus
		want       orchestrator.TaskStatus
	}{
		{"from triaged", orchestrator.TaskStatusTriaged, orchestrator.TaskStatusTriaged},
		{"from ready", orchestrator.TaskStatusReady, orchestrator.TaskStatusReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
			txStore := &recordingTxStore{
				task:         task,
				parkedFromFn: func(taskID string) (orchestrator.TaskStatus, error) { return c.parkedFrom, nil },
			}
			svc := newTriageWorkflowService(task, txStore)

			result, err := svc.Wake(context.Background(), task.ID)
			if err != nil {
				t.Fatalf("Wake: %v", err)
			}
			if result.Task.Status != c.want {
				t.Fatalf("status = %q, want %q", result.Task.Status, c.want)
			}
		})
	}
}

func TestTaskWorkflowService_Wake_RejectsNonParkedTask(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})
	if _, err := svc.Wake(context.Background(), task.ID); err == nil {
		t.Fatal("expected error waking a non-parked task")
	}
}

// TestTaskWorkflowServiceApplyAction_RejectsInternalOnlyWakeActions is the
// regression test for codex review round 1's Blocker: wake_triaged/
// wake_ready were Manual:false (hidden from AvailableActions/UI) but still
// directly reachable via the generic public ApplyAction path — i.e. the
// same route the HTTP API / brokered action_send / `boid action send` CLI
// all funnel through — letting a caller promote a parked task straight to
// ready without going through Wake's ParkedFrom-based origin check
// (bypassing the Go-gate 決定9/逆輸入2 protects).
// TestTaskWorkflowServiceApplyAction_RejectsNonManualActions is the
// regression test for codex review round 2's Blocker: a hand-maintained
// blocklist of "internal-only" action names had missed job_failed, letting
// triaged →(job_failed)→aborted →(reopen)→executing bypass the ready-gate
// entirely. ApplyAction now derives rejection generically from each rule's
// own Manual flag (StateMachine.IsManualAction) instead of a separate list.
func TestTaskWorkflowServiceApplyAction_RejectsNonManualActions(t *testing.T) {
	for _, actionType := range []string{"job_failed", "progress", "done_request", "fail_request", "wake_triaged", "wake_ready", "wake_working"} {
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
		svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

		_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: actionType})
		if err == nil {
			t.Fatalf("action %q: expected rejection via public ApplyAction, got nil error", actionType)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusBadRequest {
			t.Fatalf("action %q: expected 400 StatusError, got %v", actionType, err)
		}
	}
}

// TestJobFailedStillReachesAbortedViaCompleteJob confirms job_failed's real
// (non-ApplyAction) caller, TaskWorkflowService.CompleteJob, still works —
// the fix must reject job_failed only from the PUBLIC ApplyAction path, not
// break the legitimate internal one, which builds its own orchestrator.Action
// and calls sm.Apply directly (internal/api/workflow_job.go), never going
// through ApplyAction at all.
func TestExecutionMachine_JobFailed_StillApplicableDirectlyViaSmApply(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "job_failed"})
	if err != nil {
		t.Fatalf("job_failed via direct sm.Apply (CompleteJob's path): %v", err)
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("status = %q, want aborted", next.Status)
	}
}

func TestTaskWorkflowServiceApplyAction_RejectsInternalOnlyWakeActions(t *testing.T) {
	for _, actionType := range []string{"wake_triaged", "wake_ready", "wake_working"} {
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
		svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

		_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: actionType})
		if err == nil {
			t.Fatalf("action %q: expected rejection via public ApplyAction, got nil error", actionType)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusBadRequest {
			t.Fatalf("action %q: expected 400 StatusError, got %v", actionType, err)
		}
	}
}

func TestTaskWorkflowService_Wake_ErrorsWhenParkedFromUnknown(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task:         task,
		parkedFromFn: func(taskID string) (orchestrator.TaskStatus, error) { return "", fmt.Errorf("no park action") },
	}
	svc := newTriageWorkflowService(task, txStore)
	if _, err := svc.Wake(context.Background(), task.ID); err == nil {
		t.Fatal("expected error when ParkedFrom cannot be resolved")
	}
}

// TestTaskWorkflowService_Wake_FromReady_ChainsIntoDispatch is the regression
// test for codex review round 1's Major finding on PR-3: a task parked FROM
// ready (i.e. already Go'd once) that gets woken must not be stranded in
// ready — Wake has to chain into Dispatch exactly like ApplyAction("ready")
// does, or the task has no forward path at all (no UI button targets ready,
// and SweepWake only re-evaluates parked tasks). Deliberately uses txStore
// itself as s.Tasks (not the newTriageWorkflowService helper's separate
// stubTaskStore, which doesn't observe the transaction's write and so
// wouldn't actually exercise Dispatch's ready-status precondition check).
func TestTaskWorkflowService_Wake_FromReady_ChainsIntoDispatch(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task:         task,
		parkedFromFn: func(taskID string) (orchestrator.TaskStatus, error) { return orchestrator.TaskStatusReady, nil },
	}
	svc := &TaskWorkflowService{
		Tasks:      txStore,
		TaskTriage: txStore,
		Tx:         recordingTransactor{store: txStore},
		Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	result, err := svc.Wake(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working (wake_ready must chain into Dispatch, same as ApplyAction(\"ready\"))", result.Task.Status)
	}
}

// TestTaskWorkflowService_Wake_FromWorking_ReturnsToWorking_NoDispatch is the
// BD-9 regression test (穴1): before this fix, Wake's ParkedFrom switch had
// no case for TaskStatusWorking and fell into `default`, 500ing with
// `wake: unexpected park origin "working"` — exactly the failure the real
// khi workspace card (633c4bd9-...) hit. A working-origin park is the 論点8
// "sequential PR consumption" pattern (park + wake_task_id, re-surfaced by
// QueueSweepLoop when the dispatched child terminates), so waking it must
// land back on working, not error.
//
// It also pins that Wake's ready→working Dispatch chain (guarded by
// `newTask.Status == TaskStatusReady`, see Wake's doc comment) must NOT fire
// for this origin: unlike a ready-origin park, a working-origin park's child
// is already dispatched, so there is nothing left to (re-)dispatch. The
// spy's getCalls counter proves s.Dispatch (whose first call is
// s.Tasks.GetTask) was never even attempted — checking only the final status
// would not catch a regression here, because Dispatch's own ready-only
// precondition guard would silently no-op (log-only error) against a
// "working" task even if Wake's chain condition were mistakenly widened to
// include working.
func TestTaskWorkflowService_Wake_FromWorking_ReturnsToWorking_NoDispatch(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task:         task,
		parkedFromFn: func(taskID string) (orchestrator.TaskStatus, error) { return orchestrator.TaskStatusWorking, nil },
	}
	taskSpy := &stubTaskStore{task: task}
	svc := &TaskWorkflowService{
		Tasks:      taskSpy,
		TaskTriage: txStore,
		Tx:         recordingTransactor{store: txStore},
		Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	result, err := svc.Wake(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}
	if result.Action.Type != "wake_working" {
		t.Fatalf("action type = %q, want wake_working", result.Action.Type)
	}
	if taskSpy.getCalls != 0 {
		t.Fatalf("s.Tasks.GetTask was called %d time(s); Dispatch must never be attempted for a working-origin wake", taskSpy.getCalls)
	}
}

// TestTaskWorkflowService_Wake_ResolvesAllParkOrigins is the api-side half of
// the BD-9 recurrence guard (internal/orchestrator's
// TestCardMachine_ParkOrigins_AllHaveWakeRule pins the machine-rule half).
// The named tests above (TriagedThenReady / FromWorking_ReturnsToWorking)
// pin today's three origins by name; this one instead derives the origin set
// from NewCardMachine().Rules — deliberately not a hardcoded
// []orchestrator.TaskStatus{"triaged","ready","working"} literal — and
// end-to-end pins that Wake resolves ALL of them without error. A fourth
// park origin added without a matching case in Wake's ParkedFrom switch
// falls into that switch's `default:` branch and surfaces here as a
// StatusError, by name, instead of only surfacing as a 500 in production the
// way BD-9 did.
//
// It reuses newTriageWorkflowService (Coordinator intentionally nil), the
// same harness TestTaskWorkflowService_Wake_RoundTrip_TriagedThenReady uses.
// Under this harness, Wake's ready→working Dispatch chain always fails
// (logged, not surfaced — see Wake's doc comment): sm.Apply operates on a
// COPY of the task (machine.go's Apply: `newTask := *task`), so the
// underlying stubTaskStore still holds the pre-wake "parked" task when
// Dispatch's own ready-only precondition guard reads it back
// (workflow_triage.go's Dispatch: `if task.Status != TaskStatusReady`) and
// rejects with a 409 the caller never sees. That leaves status at "ready",
// so under this harness every origin's resulting status equals the origin
// itself regardless of whether that origin also triggers the Dispatch
// chain — which is what lets one assertion shape cover every origin
// generically instead of needing a per-origin special case.
func TestTaskWorkflowService_Wake_ResolvesAllParkOrigins(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	var origins []orchestrator.TaskStatus
	for _, r := range sm.Rules {
		if r.Action == "park" && r.ToStatus == string(orchestrator.TaskStatusParked) {
			origins = append(origins, orchestrator.TaskStatus(r.FromStatus))
		}
	}
	if len(origins) == 0 {
		t.Fatal("no park rules found in NewCardMachine().Rules — did the rule table's shape change?")
	}

	for _, origin := range origins {
		t.Run(string(origin), func(t *testing.T) {
			task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
			txStore := &recordingTxStore{
				task:         task,
				parkedFromFn: func(taskID string) (orchestrator.TaskStatus, error) { return origin, nil },
			}
			svc := newTriageWorkflowService(task, txStore)

			result, err := svc.Wake(context.Background(), task.ID)
			if err != nil {
				t.Fatalf("Wake from park origin %q: %v (Wake's ParkedFrom switch is likely missing a case for this origin — BD-9 recurrence)", origin, err)
			}
			if result.Task.Status != origin {
				t.Fatalf("Wake from park origin %q: status = %q, want %q", origin, result.Task.Status, origin)
			}
		})
	}
}

type fixedDispatchResult struct {
	result *orchestrator.DispatchResult
}

func (d fixedDispatchResult) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	return d.result, nil
}

func (d fixedDispatchResult) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

func TestTaskWorkflowServiceRunDispatchLoop_MustNotOverwriteTerminalStatusWhenPersistingPayload(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{"prompt":"start"}`),
	}
	completed := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusDone,
		Behavior:  task.Behavior,
		Payload:   task.Payload,
	}

	txStore := &recordingTxStore{task: completed}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				FinalPayload: []byte(`{"prompt":"start","artifact":{"summary":"ok"}}`),
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.NewExecutionMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusDone {
		t.Fatalf("updated task status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusDone)
	}
	if lifecycle.cleanupTaskID != task.ID {
		t.Fatalf("cleanup task id = %q, want %q", lifecycle.cleanupTaskID, task.ID)
	}
}

// If the DB shows the task as aborted by the time we come back from a hook
// dispatch that computed a NewStatus advance, the loop must drop the advance
// rather than overwriting the terminal status.
func TestTaskWorkflowServiceRunDispatchLoop_MustNotOverwriteTerminalStatusWhenAdvanceIsAvailable(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{"prompt":"start"}`),
	}
	aborted := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusAborted,
		Behavior:  task.Behavior,
		Payload:   task.Payload,
	}

	txStore := &recordingTxStore{task: aborted}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				FinalPayload: []byte(`{"prompt":"start","artifact":{"summary":"ok"}}`),
				NewStatus:    orchestrator.TaskStatusDone,
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.NewExecutionMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("updated task status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusAborted)
	}
	if lifecycle.cleanupTaskID != task.ID {
		t.Fatalf("cleanup task id = %q, want %q", lifecycle.cleanupTaskID, task.ID)
	}
	for _, a := range txStore.actions {
		if a.Type == "auto_advance" {
			t.Fatalf("unexpected auto_advance action written after abort: %+v", a)
		}
	}
}

// After a dispatch cycle, the awaiting.pending_answer must be stripped from
// the persisted payload so the answer is not re-consumed on the next hook run.
// Other awaiting fields (question / question_id) must survive so the kit can
// still see what was asked. Legacy persisted records may also carry session_id
// — the deserialiser ignores it (the field was removed) but the raw payload
// passes through unchanged.
func TestTaskWorkflowServiceRunDispatchLoop_ClearsPendingAnswerAfterDispatch(t *testing.T) {
	withAnswer := `{"awaiting":{"session_id":"sess-1","question_id":"q-1","pending_answer":"yes"}}`
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(withAnswer),
	}
	// The DB still returns the task-with-answer so the tx refresh gets it.
	taskInDB := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  task.Behavior,
		Payload:   []byte(withAnswer),
	}

	txStore := &recordingTxStore{task: taskInDB}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				// Hook wrote back the full payload unchanged.
				FinalPayload: []byte(withAnswer),
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.NewExecutionMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	ap := orchestrator.GetAwaitingPayload(txStore.updatedTask.Payload)
	if ap.PendingAnswer != "" {
		t.Errorf("pending_answer = %q, want empty after dispatch", ap.PendingAnswer)
	}
	if ap.QuestionID != "q-1" {
		t.Errorf("question_id = %q, want q-1 (must be preserved)", ap.QuestionID)
	}
}

// Regression: when a hook calls `boid task notify --ask` mid-flight, the new
// awaiting trait is written to the DB by ApplyAction("ask") *during* the hook.
// The coordinator's FinalPayload, however, derives from a snapshot of
// task.Payload taken before the hook ran, so it carries the *previous* turn's
// awaiting trait. The dispatch-loop merge must not let that stale awaiting
// clobber the freshly-persisted DB row. Bug observed: 2nd Q&A turn displayed
// the 1st turn's question text in the Web UI.
func TestTaskWorkflowServiceRunDispatchLoop_MidHookAsk_PreservesNewAwaiting(t *testing.T) {
	staleAwaiting := `{"awaiting":{"question":"OLD","question_id":"q-1","pending_answer":"approve"}}`
	freshAwaiting := `{"awaiting":{"question":"NEW","question_id":"q-2"}}`

	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(staleAwaiting),
	}
	// DB row already reflects the mid-hook ApplyAction("ask"): question_id=q-2,
	// fresh question text. Status is awaiting because the in-flight notify --ask
	// transitioned it.
	taskInDB := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusAwaiting,
		Behavior:  task.Behavior,
		Payload:   []byte(freshAwaiting),
	}

	txStore := &recordingTxStore{task: taskInDB}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				// Coordinator's snapshot still has q-1 / OLD content.
				FinalPayload: []byte(staleAwaiting),
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.NewExecutionMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	ap := orchestrator.GetAwaitingPayload(txStore.updatedTask.Payload)
	if ap.QuestionID != "q-2" {
		t.Errorf("question_id = %q, want q-2 (mid-hook ask must not be clobbered)", ap.QuestionID)
	}
	if ap.Question != "NEW" {
		t.Errorf("question = %q, want NEW (mid-hook ask must not be clobbered)", ap.Question)
	}
	if ap.PendingAnswer != "" {
		t.Errorf("pending_answer = %q, want empty (must be cleared)", ap.PendingAnswer)
	}
}
