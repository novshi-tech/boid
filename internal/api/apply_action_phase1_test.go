package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type recordingTxStore struct {
	task        *orchestrator.Task
	updatedTask *orchestrator.Task
	actions     []*orchestrator.Action
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
func (s *recordingTxStore) FindTaskByRemote(remoteID, datasourceID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) FindDependentTasks(taskID string) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) CreateAction(action *orchestrator.Action) error {
	s.actions = append(s.actions, action)
	return nil
}
func (s *recordingTxStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	return nil, nil
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

func (p *dispatchContextProbe) DispatchEntryGates(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta) (*orchestrator.EntryGateResult, error) {
	return &orchestrator.EntryGateResult{FinalPayload: task.Payload}, nil
}

func (p *dispatchContextProbe) ReplayGate(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, gateID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
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

type fixedDispatchResult struct {
	result *orchestrator.DispatchResult
}

func (d fixedDispatchResult) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	return d.result, nil
}

func (d fixedDispatchResult) DispatchEntryGates(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta) (*orchestrator.EntryGateResult, error) {
	return &orchestrator.EntryGateResult{FinalPayload: task.Payload}, nil
}

func (d fixedDispatchResult) ReplayGate(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, gateID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
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
// awaiting.session_id must survive so the kit can resume the claude session.
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
	if ap.SessionID != "sess-1" {
		t.Errorf("session_id = %q, want sess-1 (must be preserved)", ap.SessionID)
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
