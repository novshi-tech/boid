package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// scriptedTaskStore returns a different task on each GetTask call, so a test
// can drive a task through executing → terminal while WaitTaskTerminal is
// blocked on it. Embeds stubTaskStore purely to satisfy the rest of the
// TaskStore interface.
type scriptedTaskStore struct {
	*stubTaskStore
	statuses []orchestrator.TaskStatus
	calls    int
	getErr   error
}

func (s *scriptedTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	i := s.calls
	if i >= len(s.statuses) {
		i = len(s.statuses) - 1
	}
	s.calls++
	return &orchestrator.Task{
		ID:        id,
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Status:    s.statuses[i],
		Exec:      &orchestrator.ExecAttrs{},
	}, nil
}

// listingActionStore serves a fixed action list to WaitTaskTerminal's abort
// reason lookup.
type listingActionStore struct {
	actions []*orchestrator.Action
	err     error
}

func (s *listingActionStore) CreateAction(_ context.Context, _ *orchestrator.Action) error {
	return nil
}

func (s *listingActionStore) ListActionsByTask(_ string) ([]*orchestrator.Action, error) {
	return s.actions, s.err
}

func abortAction(code, message string) *orchestrator.Action {
	payload, _ := json.Marshal(map[string]string{"code": code, "message": message})
	return &orchestrator.Action{
		Type:     "abort",
		ToStatus: orchestrator.TaskStatusAborted,
		Payload:  payload,
	}
}

func waitService(tasks TaskStore, actions ActionStore) *TaskAppService {
	return &TaskAppService{
		Tasks:            tasks,
		Actions:          actions,
		WaitPollInterval: time.Millisecond,
	}
}

// A task that is already terminal must return without ever sleeping — the
// common case when a previous round finished before the trigger fired again.
func TestWaitTaskTerminal_AlreadyTerminalReturnsImmediately(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusDone},
	}
	svc := waitService(store, &listingActionStore{})

	outcome, err := svc.WaitTaskTerminal(context.Background(), "t1")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.Status != orchestrator.TaskStatusDone {
		t.Errorf("status = %q, want done", outcome.Status)
	}
	if store.calls != 1 {
		t.Errorf("GetTask calls = %d, want 1 (no polling for an already-terminal task)", store.calls)
	}
}

// The whole point of the op: stay blocked while the task is still running so
// the trigger's exec job lives as long as the work it started.
func TestWaitTaskTerminal_BlocksUntilTerminal(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses: []orchestrator.TaskStatus{
			orchestrator.TaskStatusPending,
			orchestrator.TaskStatusExecuting,
			orchestrator.TaskStatusAwaiting,
			orchestrator.TaskStatusDone,
		},
	}
	svc := waitService(store, &listingActionStore{})

	outcome, err := svc.WaitTaskTerminal(context.Background(), "t1")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.Status != orchestrator.TaskStatusDone {
		t.Errorf("status = %q, want done", outcome.Status)
	}
	if store.calls != 4 {
		t.Errorf("GetTask calls = %d, want 4 (polled until the task went terminal)", store.calls)
	}
}

// An aborted task must carry its reason out — this is what the trigger job's
// stderr shows a human in `boid job log` when the round failed.
func TestWaitTaskTerminal_AbortedCarriesReason(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusAborted},
	}
	actions := &listingActionStore{actions: []*orchestrator.Action{
		abortAction("dispatch_error", "harness が起動できなかった"),
	}}
	svc := waitService(store, actions)

	outcome, err := svc.WaitTaskTerminal(context.Background(), "t1")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("status = %q, want aborted", outcome.Status)
	}
	if outcome.AbortCode != "dispatch_error" {
		t.Errorf("abort code = %q, want dispatch_error", outcome.AbortCode)
	}
	if outcome.AbortMessage != "harness が起動できなかった" {
		t.Errorf("abort message = %q", outcome.AbortMessage)
	}
}

// A task that was reopened after an earlier abort must report the abort that
// just happened, not the first one in its history. orchestrator.DeriveLifecycle
// deliberately keeps the FIRST abort (lifecycle.go's `lc.Abort == nil` guard,
// and unlike Done/Fail it is never reset on a reopen), which is correct for a
// task that aborts once and stays terminal but wrong for a waited-on task that
// gets reopened — hence this op reads the action list itself.
func TestWaitTaskTerminal_AbortReasonIsTheMostRecentAbort(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusAborted},
	}
	actions := &listingActionStore{actions: []*orchestrator.Action{
		abortAction("dispatch_error", "古い失敗"),
		{Type: "reopen", ToStatus: orchestrator.TaskStatusExecuting},
		abortAction("", "hook が非ゼロで終了した"),
	}}
	svc := waitService(store, actions)

	outcome, err := svc.WaitTaskTerminal(context.Background(), "t1")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.AbortCode != "" {
		t.Errorf("abort code = %q, want \"\" (the latest abort, a session failure)", outcome.AbortCode)
	}
	if outcome.AbortMessage != "hook が非ゼロで終了した" {
		t.Errorf("abort message = %q, want the latest abort's message", outcome.AbortMessage)
	}
}

// The reason lookup must never turn a real terminal outcome into an error: the
// status is the answer the caller needs, the reason is decoration.
func TestWaitTaskTerminal_ActionLookupFailureStillReportsTheStatus(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusAborted},
	}
	actions := &listingActionStore{err: errors.New("db is down")}
	svc := waitService(store, actions)

	outcome, err := svc.WaitTaskTerminal(context.Background(), "t1")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.Status != orchestrator.TaskStatusAborted {
		t.Errorf("status = %q, want aborted", outcome.Status)
	}
}

// Cancellation is the only thing that stops the wait short — the broker cancels
// on daemon shutdown / sandbox disconnect, so this is what keeps the op from
// holding a goroutine forever.
func TestWaitTaskTerminal_ContextCancelled(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusExecuting},
	}
	svc := waitService(store, &listingActionStore{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	if _, err := svc.WaitTaskTerminal(ctx, "t1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A task id that does not resolve is an error, not an endless wait — otherwise a
// typo'd id would hold the trigger's exec job open until its timeout.
func TestWaitTaskTerminal_MissingTaskIsAnError(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusExecuting},
		getErr:        errors.New("task not found"),
	}
	svc := waitService(store, &listingActionStore{})

	if _, err := svc.WaitTaskTerminal(context.Background(), "t1"); err == nil {
		t.Fatal("expected an error for an unresolvable task id")
	}
}

// An empty id must fail fast rather than reach the store.
func TestWaitTaskTerminal_EmptyIDIsAnError(t *testing.T) {
	svc := waitService(&scriptedTaskStore{stubTaskStore: &stubTaskStore{}}, &listingActionStore{})
	if _, err := svc.WaitTaskTerminal(context.Background(), ""); err == nil {
		t.Fatal("expected an error for an empty task id")
	}
}
