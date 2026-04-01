package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type recordingTxStore struct {
	updatedTask *orchestrator.Task
	actions     []*orchestrator.Action
}

func (s *recordingTxStore) CreateTask(task *orchestrator.Task) error { return nil }
func (s *recordingTxStore) GetTask(id string) (*orchestrator.Task, error) {
	return nil, fmt.Errorf("not implemented")
}
func (s *recordingTxStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) UpdateTask(task *orchestrator.Task) error {
	s.updatedTask = task
	return nil
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

func (p *dispatchContextProbe) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, behavior *orchestrator.TaskBehavior, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
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
		Tasks:    &stubTaskStore{task: task},
		Tx:       recordingTransactor{store: txStore},
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver: stubResolver{sm: orchestrator.OneShotMachine()},
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
