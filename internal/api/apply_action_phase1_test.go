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
}

func (s *recordingTxStore) CreateTask(task *orchestrator.Task) error { return nil }
func (s *recordingTxStore) GetTask(id string) (*orchestrator.Task, error) {
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
func (s *recordingTxStore) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) ListChildren(parentID string) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) CreateAction(action *orchestrator.Action) error {
	s.actions = append(s.actions, action)
	return nil
}
func (s *recordingTxStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	return nil, nil
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

func newTriageWorkflowService(task *orchestrator.Task, txStore *recordingTxStore) *TaskWorkflowService {
	return &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		// Coordinator intentionally nil: pre-execution transitions have no
		// hooks to dispatch (no runtime exists for a triage task).
	}
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
	for _, actionType := range []string{"job_failed", "progress", "done_request", "fail_request", "wake_triaged", "wake_ready"} {
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
func TestDefaultMachine_JobFailed_StillApplicableDirectlyViaSmApply(t *testing.T) {
	sm := orchestrator.DefaultMachine()
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
	for _, actionType := range []string{"wake_triaged", "wake_ready"} {
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
		orchestrator.DefaultMachine(),
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
		orchestrator.DefaultMachine(),
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
		orchestrator.DefaultMachine(),
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
		orchestrator.DefaultMachine(),
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
