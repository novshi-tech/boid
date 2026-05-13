package api

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// holdingDispatchCoordinator runs DispatchAndAdvance synchronously and blocks
// on a channel so tests can observe the "while-executing" state.
type holdingDispatchCoordinator struct {
	enter   chan struct{}
	release chan struct{}
	result  *orchestrator.DispatchResult
}

func newHoldingDispatchCoordinator(result *orchestrator.DispatchResult) *holdingDispatchCoordinator {
	return &holdingDispatchCoordinator{
		enter:   make(chan struct{}, 1),
		release: make(chan struct{}),
		result:  result,
	}
}

func (h *holdingDispatchCoordinator) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	select {
	case h.enter <- struct{}{}:
	default:
	}
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if h.result != nil {
		return h.result, nil
	}
	return &orchestrator.DispatchResult{FinalPayload: task.Payload}, nil
}

func (h *holdingDispatchCoordinator) DispatchEntryGates(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta) (*orchestrator.EntryGateResult, error) {
	return &orchestrator.EntryGateResult{FinalPayload: task.Payload}, nil
}

func (h *holdingDispatchCoordinator) ReplayGate(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, gateID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

func (h *holdingDispatchCoordinator) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

// TestProjectLock_RunDispatchLoop_AcquiresAndReleasesOnDone verifies the
// happy path: the lock is acquired at runDispatchLoop entry and released
// once the task reaches done via auto-advance + finalizeTerminal.
func TestProjectLock_RunDispatchLoop_AcquiresAndReleasesOnDone(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	doneInDB := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusDone,
		Behavior:  task.Behavior,
		Payload:   task.Payload,
	}

	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	txStore := &recordingTxStore{task: doneInDB}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{FinalPayload: task.Payload},
		},
		Lifecycle: &stubLifecycle{},
		Locks:     locks,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.DefaultMachine(),
	)

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock released after reaching terminal status")
	}
}

// TestProjectLock_RunDispatchLoop_SkipsLockForReadonly verifies that readonly
// tasks bypass the project lock entirely (matches the legacy
// dispatchHooksLocked eligibility check).
func TestProjectLock_RunDispatchLoop_SkipsLockForReadonly(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-ro",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "plan",
		Readonly:  true,
		Payload:   []byte(`{}`),
	}
	doneInDB := *task
	doneInDB.Status = orchestrator.TaskStatusDone

	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	txStore := &recordingTxStore{task: &doneInDB}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{FinalPayload: task.Payload},
		},
		Lifecycle: &stubLifecycle{},
		Locks:     locks,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.DefaultMachine(),
	)

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("readonly task must not hold the project lock")
	}
}

// TestProjectLock_RunDispatchLoop_SkipsLockForWorktreeTask verifies that
// worktree=true tasks bypass the project lock.
func TestProjectLock_RunDispatchLoop_SkipsLockForWorktreeTask(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-wt",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Worktree:  true,
		Payload:   []byte(`{}`),
	}
	doneInDB := *task
	doneInDB.Status = orchestrator.TaskStatusDone

	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	txStore := &recordingTxStore{task: &doneInDB}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{FinalPayload: task.Payload},
		},
		Lifecycle: &stubLifecycle{},
		Locks:     locks,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.DefaultMachine(),
	)

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("worktree=true task must not hold the project lock")
	}
}

// TestProjectLock_RunDispatchLoop_HeldDuringDispatch verifies that the lock
// remains held while the dispatch coordinator is in flight. Uses a holding
// coordinator that blocks until released.
func TestProjectLock_RunDispatchLoop_HeldDuringDispatch(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	doneInDB := *task
	doneInDB.Status = orchestrator.TaskStatusDone

	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	holding := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: task.Payload})
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &doneInDB}},
		Coordinator: holding,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())
	}()

	select {
	case <-holding.enter:
	case <-time.After(time.Second):
		t.Fatal("dispatch coordinator never entered")
	}

	if !locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock held while dispatch coordinator is running")
	}

	close(holding.release)
	<-done

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock released after dispatch loop completed")
	}
}

// TestProjectLock_RunDispatchLoop_HeldUntilTerminal_BlocksConcurrentTask
// is the central correctness test: while task A is executing on project P,
// task B on the same project blocks at lock acquire. Once A reaches done,
// B proceeds.
func TestProjectLock_RunDispatchLoop_HeldUntilTerminal_BlocksConcurrentTask(t *testing.T) {
	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	taskA := &orchestrator.Task{
		ID:        "task-a",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	taskADoneInDB := *taskA
	taskADoneInDB.Status = orchestrator.TaskStatusDone

	holdingA := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: taskA.Payload})
	svcA := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &taskADoneInDB}},
		Coordinator: holdingA,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		svcA.runDispatchLoop(context.Background(), taskA, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())
	}()

	// Wait for A to enter dispatch and confirm it holds the lock.
	select {
	case <-holdingA.enter:
	case <-time.After(time.Second):
		t.Fatal("task A dispatch did not enter")
	}
	if !locks.IsHeldForTask("task-a") {
		t.Fatal("task A should hold the lock")
	}

	// Try B on the same project — should block at AcquireForTask.
	taskB := &orchestrator.Task{
		ID:        "task-b",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	taskBDoneInDB := *taskB
	taskBDoneInDB.Status = orchestrator.TaskStatusDone

	holdingB := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: taskB.Payload})
	svcB := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &taskBDoneInDB}},
		Coordinator: holdingB,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		svcB.runDispatchLoop(context.Background(), taskB, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())
	}()

	select {
	case <-holdingB.enter:
		t.Fatal("task B entered dispatch while task A held the lock")
	case <-time.After(80 * time.Millisecond):
		// expected — B is blocked at AcquireForTask
	}

	// Release A — terminal status will release the lock.
	close(holdingA.release)
	<-doneA
	if locks.IsHeldForTask("task-a") {
		t.Fatal("task A lock should be released after done")
	}

	// Now B should make progress.
	select {
	case <-holdingB.enter:
		// expected
	case <-time.After(time.Second):
		t.Fatal("task B never entered dispatch after A released")
	}
	if !locks.IsHeldForTask("task-b") {
		t.Fatal("task B should hold the lock after acquiring")
	}
	close(holdingB.release)
	<-doneB

	if locks.IsHeldForTask("task-b") {
		t.Fatal("task B lock should be released after done")
	}
}

// TestProjectLock_RunDispatchLoop_DifferentProjectsInParallel verifies that
// tasks on distinct projects do NOT serialize.
func TestProjectLock_RunDispatchLoop_DifferentProjectsInParallel(t *testing.T) {
	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	taskA := &orchestrator.Task{
		ID:        "task-a",
		ProjectID: "proj-A",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	taskADone := *taskA
	taskADone.Status = orchestrator.TaskStatusDone
	holdingA := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: taskA.Payload})
	svcA := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &taskADone}},
		Coordinator: holdingA,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	taskB := &orchestrator.Task{
		ID:        "task-b",
		ProjectID: "proj-B",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	taskBDone := *taskB
	taskBDone.Status = orchestrator.TaskStatusDone
	holdingB := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: taskB.Payload})
	svcB := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &taskBDone}},
		Coordinator: holdingB,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() {
		defer close(doneA)
		svcA.runDispatchLoop(context.Background(), taskA, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())
	}()
	go func() {
		defer close(doneB)
		svcB.runDispatchLoop(context.Background(), taskB, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())
	}()

	// Both should enter dispatch concurrently.
	for i := 0; i < 2; i++ {
		select {
		case <-holdingA.enter:
		case <-holdingB.enter:
		case <-time.After(time.Second):
			t.Fatal("both tasks should enter dispatch concurrently on different projects")
		}
	}

	close(holdingA.release)
	close(holdingB.release)
	<-doneA
	<-doneB
}

// TestProjectLock_RunDispatchLoop_ReleasesOnAwaiting verifies that the lock
// is released when the task transitions to awaiting via mid-hook ask.
func TestProjectLock_RunDispatchLoop_ReleasesOnAwaiting(t *testing.T) {
	awaiting := `{"awaiting":{"question":"q?","question_id":"q-1"}}`
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	awaitingInDB := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusAwaiting,
		Behavior:  task.Behavior,
		Payload:   []byte(awaiting),
	}

	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	txStore := &recordingTxStore{task: awaitingInDB}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{FinalPayload: []byte(`{}`)},
		},
		Lifecycle: &stubLifecycle{},
		Locks:     locks,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.DefaultMachine(),
	)

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock released after awaiting transition")
	}
}

// TestProjectLock_ApplyAction_ReleasesOnLeavingExecuting verifies that
// ApplyAction releases the lock on transitions out of executing (e.g. abort)
// even when the dispatch loop hasn't had a chance to release it yet.
func TestProjectLock_ApplyAction_ReleasesOnAbort(t *testing.T) {
	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	// Pre-acquire the lock to simulate a task that is mid-execution.
	if err := locks.AcquireForTask(context.Background(), "proj-1", "task-1"); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	if !locks.IsHeldForTask("task-1") {
		t.Fatal("lock should be held after pre-acquire")
	}

	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		// No coordinator → no background dispatch loop.
		Locks: locks,
	}

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "abort"}); err != nil {
		t.Fatalf("abort: %v", err)
	}

	if locks.IsHeldForTask("task-1") {
		t.Fatal("expected lock released after abort moved task out of executing")
	}
}

// TestProjectLock_ApplyAction_ReleasesOnAsk verifies that ask (executing →
// awaiting) releases the lock so other tasks can run on the project.
func TestProjectLock_ApplyAction_ReleasesOnAsk(t *testing.T) {
	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	if err := locks.AcquireForTask(context.Background(), "proj-1", "task-1"); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}

	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		Locks: locks,
	}

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "ask"}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if locks.IsHeldForTask("task-1") {
		t.Fatal("expected lock released after ask moved task to awaiting")
	}
}
