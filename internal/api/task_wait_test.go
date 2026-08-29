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
	// resolvedID, when set, is what GetTask reports as the task's id — the
	// prefix-resolution orchestrator.GetTask performs for ids >= 8 chars.
	resolvedID string
	// statusErrs is consulted by GetTaskStatus before statuses: a non-nil entry
	// makes that poll fail, so a test can inject a transient read failure.
	statusErrs map[int]error
	// statusCalls counts the narrow poll separately from the initial resolve.
	statusCalls int
}

func (s *scriptedTaskStore) statusAt(i int) orchestrator.TaskStatus {
	if i >= len(s.statuses) {
		i = len(s.statuses) - 1
	}
	return s.statuses[i]
}

func (s *scriptedTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	i := s.calls
	s.calls++
	resolved := s.resolvedID
	if resolved == "" {
		resolved = id
	}
	return &orchestrator.Task{
		ID:        resolved,
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Status:    s.statusAt(i),
		Exec:      &orchestrator.ExecAttrs{},
	}, nil
}

// GetTaskStatus makes this stub a TaskStatusReader, so the tests exercise the
// same narrow-read path production takes rather than the GetTask fallback.
func (s *scriptedTaskStore) GetTaskStatus(id string) (orchestrator.TaskStatus, error) {
	i := s.calls + s.statusCalls
	s.statusCalls++
	if err, ok := s.statusErrs[s.statusCalls]; ok {
		return "", err
	}
	return s.statusAt(i), nil
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
	if store.statusCalls != 0 {
		t.Errorf("status polls = %d, want 0", store.statusCalls)
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
	// One full resolve, then narrow polls only — the expensive read must not
	// be what a wait repeats (orchestrator.GetTask costs five unindexed scans
	// of the tasks table, on a single-connection pool).
	if store.calls != 1 {
		t.Errorf("GetTask calls = %d, want 1 (resolve only)", store.calls)
	}
	if store.statusCalls != 3 {
		t.Errorf("status polls = %d, want 3 (polled until the task went terminal)", store.statusCalls)
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

// `dropped` is terminal but is NOT success. Without this, mutating Succeeded to
// `Status != aborted` survives every other test here and a dropped round would
// silently report as a clean one.
func TestWaitTaskTerminal_DroppedIsTerminalButNotSuccess(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusDropped},
	}
	svc := waitService(store, &listingActionStore{})

	outcome, err := svc.WaitTaskTerminal(context.Background(), "t1")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.Status != orchestrator.TaskStatusDropped {
		t.Fatalf("status = %q, want dropped", outcome.Status)
	}
	if outcome.Succeeded() {
		t.Error("a dropped task must not count as success")
	}
}

// The id is resolved ONCE, up front, and everything after polls the resolved
// value. orchestrator.GetTask accepts an id prefix (>= 8 chars); re-reading with
// the prefix would re-run that fallback scan on every poll, and callers that
// compare against the id they passed would be comparing the wrong string.
func TestWaitTaskTerminal_ResolvesThePrefixOnceAndReportsTheFullID(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusExecuting, orchestrator.TaskStatusDone},
		resolvedID:    "abcdef12-3456-7890-abcd-ef1234567890",
	}
	svc := waitService(store, &listingActionStore{})

	outcome, err := svc.WaitTaskTerminal(context.Background(), "abcdef12")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.ID != "abcdef12-3456-7890-abcd-ef1234567890" {
		t.Errorf("outcome id = %q, want the resolved full id", outcome.ID)
	}
	if store.calls != 1 {
		t.Errorf("GetTask calls = %d, want 1 — the prefix must be resolved once, not on every poll", store.calls)
	}
}

// A transient read failure must NOT end the wait. Ending it fails the caller's
// job, which releases the trigger's single-flight and lets the next tick start a
// second concurrent round of work that is still running — the exact property
// this op exists to establish, undone by one unlucky read on a pool that is a
// single shared connection.
func TestWaitTaskTerminal_TransientReadErrorIsRetried(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses: []orchestrator.TaskStatus{
			orchestrator.TaskStatusExecuting,
			orchestrator.TaskStatusExecuting,
			orchestrator.TaskStatusDone,
		},
		statusErrs: map[int]error{1: errors.New("database is locked")},
	}
	svc := waitService(store, &listingActionStore{})

	outcome, err := svc.WaitTaskTerminal(context.Background(), "t1")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.Status != orchestrator.TaskStatusDone {
		t.Errorf("status = %q, want done — a locked-database read must be retried, not reported", outcome.Status)
	}
}

// A row that disappeared mid-wait (deleted / GC'd) will not come back under this
// id, so unlike a transient failure it must end the wait rather than spin.
func TestWaitTaskTerminal_TaskVanishingMidWaitEndsTheWait(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusExecuting},
		statusErrs:    map[int]error{1: orchestrator.ErrTaskNotFound},
	}
	svc := waitService(store, &listingActionStore{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := svc.WaitTaskTerminal(ctx, "t1"); err == nil {
		t.Fatal("expected an error once the task row is gone")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the wait spun instead of ending when the task row vanished")
	}
}

// A failure inside the agent's own session is recorded as job_failed with no
// payload, which CreateAction stores as `{}` — the reason comes out empty and
// must not become an error or a bogus code. Uses the raw `{}` the store really
// writes rather than a populated-but-empty payload.
func TestWaitTaskTerminal_SessionFailureAbortHasNoReason(t *testing.T) {
	store := &scriptedTaskStore{
		stubTaskStore: &stubTaskStore{},
		statuses:      []orchestrator.TaskStatus{orchestrator.TaskStatusAborted},
	}
	actions := &listingActionStore{actions: []*orchestrator.Action{
		{Type: "job_failed", ToStatus: orchestrator.TaskStatusAborted, Payload: []byte("{}")},
	}}
	svc := waitService(store, actions)

	outcome, err := svc.WaitTaskTerminal(context.Background(), "t1")
	if err != nil {
		t.Fatalf("WaitTaskTerminal: %v", err)
	}
	if outcome.AbortCode != "" || outcome.AbortMessage != "" {
		t.Errorf("reason = %q/%q, want both empty", outcome.AbortCode, outcome.AbortMessage)
	}
	if FormatAbortReason(outcome) != "" {
		t.Errorf("FormatAbortReason = %q, want empty", FormatAbortReason(outcome))
	}
}
