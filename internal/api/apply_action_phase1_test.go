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
		Tx:        recordingTransactor{store: txStore},
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
