package api

// Branch-lock acquisition retirement (docs/plans/git-gateway-cutover.md PR6
// cutover, 2026-07-10): runDispatchLoop no longer calls
// orchestrator.BranchLockManager.AcquireForTask — every project-visible job
// now clones the project fresh inside the sandbox instead of sharing a host
// git worktree, so the physical "only one worktree can check out a branch"
// constraint that motivated same-branch serialization no longer exists. Same-
// branch concurrent pushes are now resolved the ordinary git way (non-fast-
// forward reject, then fetch + merge/rebase + retry). This also removes the
// long-lived-supervisor head-of-line blocking documented in memory as
// khi-supervisor-branch-lock-headline-block.
//
// s.Locks / BranchLockManager itself is left wired (releaseProjectLock stays
// a documented no-op call on every executing-leaving path) — only the
// ACQUIRE call site is gone. Deleting project_lock.go and this file outright
// is PR8's job, alongside the rest of the worktree-era machinery, so a git
// revert of this PR restores locking cleanly.
//
// The tests below were rewritten (not deleted) to assert the *new* behavior:
// AcquireForTask is never reached regardless of task shape (root/child,
// readonly/writable, worktree true/false, hookless), and same-base_branch
// tasks that used to serialize now run fully concurrently. The plain
// "lock released on $transition" tests were left as-is: ReleaseForTask on an
// unheld task is a documented no-op, so they still pass and still exercise
// the surrounding status-transition behavior.
import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// errorDispatchCoordinator is a coordinator that always returns an error from
// DispatchAndAdvance so tests can exercise the dispatch-error abort path.
type errorDispatchCoordinator struct {
	err error
}

func (e *errorDispatchCoordinator) DispatchAndAdvance(_ context.Context, task *orchestrator.Task, _ *orchestrator.ProjectMeta, _ *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	return nil, e.err
}

func (e *errorDispatchCoordinator) ReplayHook(_ context.Context, task *orchestrator.Task, _ *orchestrator.ProjectMeta, _ *orchestrator.StateMachine, _ string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

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

func (h *holdingDispatchCoordinator) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

// anyMeta returns a minimal ProjectMeta for tests that don't exercise hook behavior.
func anyMeta(behaviorName string) *orchestrator.ProjectMeta {
	return &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			behaviorName: {},
		},
	}
}

// TestBranchLock_RunDispatchLoop_NeverHeldOnDone verifies the happy path
// runs to completion (done via auto-advance + finalizeTerminal) without ever
// holding the branch lock, now that AcquireForTask is retired.
func TestBranchLock_RunDispatchLoop_NeverHeldOnDone(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "task-1",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
	}
	doneInDB := &orchestrator.Task{
		ID:         task.ID,
		ProjectID:  task.ProjectID,
		BaseBranch: task.BaseBranch,
		Status:     orchestrator.TaskStatusDone,
		Behavior:   task.Behavior,
		Payload:    task.Payload,
	}

	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	txStore := &recordingTxStore{task: doneInDB}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{FinalPayload: task.Payload},
		},
		Lifecycle: &stubLifecycle{},
		Locks:     locks,
	}

	svc.runDispatchLoop(context.Background(), task, anyMeta("impl"), orchestrator.DefaultMachine())

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock released after reaching terminal status")
	}
}

// TestBranchLock_RunDispatchLoop_SkipsLockForReadonly verifies that readonly
// tasks (supervisor) do not acquire the branch lock, so they do not block
// concurrent executor tasks on the same base_branch.
func TestBranchLock_RunDispatchLoop_SkipsLockForReadonly(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "task-ro",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "plan",
		Readonly:   true,
		Payload:    []byte(`{}`),
	}
	doneInDB := *task
	doneInDB.Status = orchestrator.TaskStatusDone

	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
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
		svc.runDispatchLoop(context.Background(), task, anyMeta("plan"), orchestrator.DefaultMachine())
	}()

	select {
	case <-holding.enter:
	case <-time.After(time.Second):
		t.Fatal("dispatch coordinator never entered")
	}

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("readonly task must not acquire the branch lock")
	}

	close(holding.release)
	<-done
}

// TestBranchLock_ReadonlySup_WritableExec_SameBaseBranch_Parallel verifies
// that a readonly supervisor and a writable executor on the same base_branch
// run concurrently — the supervisor must not block the executor.
func TestBranchLock_ReadonlySup_WritableExec_SameBaseBranch_Parallel(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	supTask := &orchestrator.Task{
		ID:         "sup-task",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "supervisor",
		Readonly:   true,
		Payload:    []byte(`{}`),
	}
	supDone := *supTask
	supDone.Status = orchestrator.TaskStatusDone
	holdingSup := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: supTask.Payload})
	svcSup := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &supDone}},
		Coordinator: holdingSup,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	execTask := &orchestrator.Task{
		ID:         "exec-task",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "executor",
		Readonly:   false,
		Payload:    []byte(`{}`),
	}
	execDone := *execTask
	execDone.Status = orchestrator.TaskStatusDone
	holdingExec := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: execTask.Payload})
	svcExec := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &execDone}},
		Coordinator: holdingExec,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	doneSup := make(chan struct{})
	doneExec := make(chan struct{})
	go func() {
		defer close(doneSup)
		svcSup.runDispatchLoop(context.Background(), supTask, anyMeta("supervisor"), orchestrator.DefaultMachine())
	}()
	go func() {
		defer close(doneExec)
		svcExec.runDispatchLoop(context.Background(), execTask, anyMeta("executor"), orchestrator.DefaultMachine())
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-holdingSup.enter:
		case <-holdingExec.enter:
		case <-time.After(time.Second):
			t.Fatal("readonly supervisor and writable executor must run concurrently on the same base_branch")
		}
	}

	close(holdingSup.release)
	close(holdingExec.release)
	<-doneSup
	<-doneExec
}

// TestBranchLock_RunDispatchLoop_NeverAcquiresForWorktreeTask verifies that
// a worktree=true child task — which pre-cutover would have acquired the
// branch lock under its unique boid/<id8> key — does not acquire it now that
// AcquireForTask is retired (see package doc comment).
func TestBranchLock_RunDispatchLoop_NeverAcquiresForWorktreeTask(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "abcd1234-0000-0000-0000-000000000000",
		ProjectID: "proj-1",
		ParentID:  "parent-task-id",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Worktree:  true,
		Payload:   []byte(`{}`),
	}
	doneInDB := *task
	doneInDB.Status = orchestrator.TaskStatusDone

	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
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
		svc.runDispatchLoop(context.Background(), task, anyMeta("dev"), orchestrator.DefaultMachine())
	}()

	select {
	case <-holding.enter:
	case <-time.After(time.Second):
		t.Fatal("dispatch coordinator never entered")
	}

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("worktree=true task must not acquire the branch lock (AcquireForTask is retired)")
	}

	close(holding.release)
	<-done
}

// TestBranchLock_RunDispatchLoop_NeverAcquiresForHooklessBehavior verifies
// that a hookless behavior — which pre-cutover would have acquired the
// branch lock like any other task — does not acquire it now that
// AcquireForTask is retired (see package doc comment).
func TestBranchLock_RunDispatchLoop_NeverAcquiresForHooklessBehavior(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "task-smoke",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "smoke",
		Payload:    []byte(`{}`),
	}
	doneInDB := *task
	doneInDB.Status = orchestrator.TaskStatusDone

	hooklessMeta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"smoke": {}, // no hooks
		},
	}

	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
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
		svc.runDispatchLoop(context.Background(), task, hooklessMeta, orchestrator.DefaultMachine())
	}()

	select {
	case <-holding.enter:
	case <-time.After(time.Second):
		t.Fatal("dispatch coordinator never entered")
	}

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("hookless behavior must not acquire the branch lock (AcquireForTask is retired)")
	}

	close(holding.release)
	<-done
}

// TestBranchLock_RunDispatchLoop_NeverHeldDuringDispatch verifies that the
// lock is never held — before, during, or after — while the dispatch
// coordinator is in flight, now that AcquireForTask is retired.
func TestBranchLock_RunDispatchLoop_NeverHeldDuringDispatch(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "task-1",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
	}
	doneInDB := *task
	doneInDB.Status = orchestrator.TaskStatusDone

	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
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
		svc.runDispatchLoop(context.Background(), task, anyMeta("impl"), orchestrator.DefaultMachine())
	}()

	select {
	case <-holding.enter:
	case <-time.After(time.Second):
		t.Fatal("dispatch coordinator never entered")
	}

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock never held while dispatch coordinator is running (AcquireForTask is retired)")
	}

	close(holding.release)
	<-done

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock still not held after dispatch loop completed")
	}
}

// TestBranchLock_SameBaseBranch_NoLongerSerializes is the central regression
// test for the retirement: two root sup tasks on the same project and same
// base_branch used to serialize (task B blocked until task A released the
// lock); post-cutover both enter dispatch concurrently with no ordering
// dependency at all. This is the fix for the long-lived-supervisor head-of-
// line blocking documented in memory as
// khi-supervisor-branch-lock-headline-block.
func TestBranchLock_SameBaseBranch_NoLongerSerializes(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	taskA := &orchestrator.Task{
		ID:         "task-a",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
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

	taskB := &orchestrator.Task{
		ID:         "task-b",
		ProjectID:  "proj-1",
		BaseBranch: "main", // same base_branch — used to be the same lock key
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
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

	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() {
		defer close(doneA)
		svcA.runDispatchLoop(context.Background(), taskA, anyMeta("impl"), orchestrator.DefaultMachine())
	}()
	go func() {
		defer close(doneB)
		svcB.runDispatchLoop(context.Background(), taskB, anyMeta("impl"), orchestrator.DefaultMachine())
	}()

	// Both must enter concurrently — neither blocks on the other, unlike the
	// pre-cutover behavior where B would have waited for A's lock release.
	for i := 0; i < 2; i++ {
		select {
		case <-holdingA.enter:
		case <-holdingB.enter:
		case <-time.After(time.Second):
			t.Fatal("same base_branch tasks should enter dispatch concurrently now that AcquireForTask is retired")
		}
	}
	if locks.IsHeldForTask("task-a") || locks.IsHeldForTask("task-b") {
		t.Fatal("neither task should hold the branch lock")
	}

	close(holdingA.release)
	close(holdingB.release)
	<-doneA
	<-doneB
}

// TestBranchLock_DifferentBaseBranches_Parallel verifies that two root sup
// tasks on the same project but different base_branches run in parallel.
func TestBranchLock_DifferentBaseBranches_Parallel(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	taskA := &orchestrator.Task{
		ID:         "task-a",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
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
		ID:         "task-b",
		ProjectID:  "proj-1",
		BaseBranch: "feature", // different base_branch → different lock key
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
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
		svcA.runDispatchLoop(context.Background(), taskA, anyMeta("impl"), orchestrator.DefaultMachine())
	}()
	go func() {
		defer close(doneB)
		svcB.runDispatchLoop(context.Background(), taskB, anyMeta("impl"), orchestrator.DefaultMachine())
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-holdingA.enter:
		case <-holdingB.enter:
		case <-time.After(time.Second):
			t.Fatal("tasks on different base_branches should enter dispatch concurrently")
		}
	}

	close(holdingA.release)
	close(holdingB.release)
	<-doneA
	<-doneB
}

// TestBranchLock_RootSupAndChildExec_Parallel verifies that a root sup task
// and a child exec task run in parallel (different branch keys).
func TestBranchLock_RootSupAndChildExec_Parallel(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	rootTask := &orchestrator.Task{
		ID:         "root-task",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		ParentID:   "", // root sup
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "supervisor",
		Payload:    []byte(`{}`),
	}
	rootDone := *rootTask
	rootDone.Status = orchestrator.TaskStatusDone
	holdingRoot := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: rootTask.Payload})
	svcRoot := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &rootDone}},
		Coordinator: holdingRoot,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	childTask := &orchestrator.Task{
		ID:        "abcd1234-0000-0000-0000-000000000000",
		ProjectID: "proj-1",
		ParentID:  "root-task", // child exec
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
		Payload:   []byte(`{}`),
	}
	childDone := *childTask
	childDone.Status = orchestrator.TaskStatusDone
	holdingChild := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: childTask.Payload})
	svcChild := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &childDone}},
		Coordinator: holdingChild,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	doneRoot := make(chan struct{})
	doneChild := make(chan struct{})
	go func() {
		defer close(doneRoot)
		svcRoot.runDispatchLoop(context.Background(), rootTask, anyMeta("supervisor"), orchestrator.DefaultMachine())
	}()
	go func() {
		defer close(doneChild)
		svcChild.runDispatchLoop(context.Background(), childTask, anyMeta("executor"), orchestrator.DefaultMachine())
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-holdingRoot.enter:
		case <-holdingChild.enter:
		case <-time.After(time.Second):
			t.Fatal("root sup and child exec should run concurrently (different branch keys)")
		}
	}

	close(holdingRoot.release)
	close(holdingChild.release)
	<-doneRoot
	<-doneChild
}

// TestBranchLock_TwoChildren_Parallel verifies that two child tasks under the
// same parent run in parallel (each has a unique boid/<id8> branch key).
func TestBranchLock_TwoChildren_Parallel(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	childA := &orchestrator.Task{
		ID:        "aaaa1111-0000-0000-0000-000000000000",
		ProjectID: "proj-1",
		ParentID:  "parent-task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
		Payload:   []byte(`{}`),
	}
	childADone := *childA
	childADone.Status = orchestrator.TaskStatusDone
	holdingA := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: childA.Payload})
	svcA := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &childADone}},
		Coordinator: holdingA,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	childB := &orchestrator.Task{
		ID:        "bbbb2222-0000-0000-0000-000000000000",
		ProjectID: "proj-1",
		ParentID:  "parent-task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
		Payload:   []byte(`{}`),
	}
	childBDone := *childB
	childBDone.Status = orchestrator.TaskStatusDone
	holdingB := newHoldingDispatchCoordinator(&orchestrator.DispatchResult{FinalPayload: childB.Payload})
	svcB := &TaskWorkflowService{
		Tx:          recordingTransactor{store: &recordingTxStore{task: &childBDone}},
		Coordinator: holdingB,
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() {
		defer close(doneA)
		svcA.runDispatchLoop(context.Background(), childA, anyMeta("executor"), orchestrator.DefaultMachine())
	}()
	go func() {
		defer close(doneB)
		svcB.runDispatchLoop(context.Background(), childB, anyMeta("executor"), orchestrator.DefaultMachine())
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-holdingA.enter:
		case <-holdingB.enter:
		case <-time.After(time.Second):
			t.Fatal("two child tasks should run concurrently (distinct boid/<id8> keys)")
		}
	}

	close(holdingA.release)
	close(holdingB.release)
	<-doneA
	<-doneB
}

// TestBranchLock_DifferentProjectsInParallel verifies that tasks on distinct
// projects do not serialize.
func TestBranchLock_DifferentProjectsInParallel(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	taskA := &orchestrator.Task{
		ID:         "task-a",
		ProjectID:  "proj-A",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
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
		ID:         "task-b",
		ProjectID:  "proj-B",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
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
		svcA.runDispatchLoop(context.Background(), taskA, anyMeta("impl"), orchestrator.DefaultMachine())
	}()
	go func() {
		defer close(doneB)
		svcB.runDispatchLoop(context.Background(), taskB, anyMeta("impl"), orchestrator.DefaultMachine())
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-holdingA.enter:
		case <-holdingB.enter:
		case <-time.After(time.Second):
			t.Fatal("tasks on different projects should run concurrently")
		}
	}

	close(holdingA.release)
	close(holdingB.release)
	<-doneA
	<-doneB
}

// TestBranchLock_RunDispatchLoop_ReleasesOnAwaiting verifies that the lock
// is released when the task transitions to awaiting via mid-hook ask.
func TestBranchLock_RunDispatchLoop_ReleasesOnAwaiting(t *testing.T) {
	awaiting := `{"awaiting":{"question":"q?","question_id":"q-1"}}`
	task := &orchestrator.Task{
		ID:         "task-1",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
	}
	awaitingInDB := &orchestrator.Task{
		ID:         task.ID,
		ProjectID:  task.ProjectID,
		BaseBranch: task.BaseBranch,
		Status:     orchestrator.TaskStatusAwaiting,
		Behavior:   task.Behavior,
		Payload:    []byte(awaiting),
	}

	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	txStore := &recordingTxStore{task: awaitingInDB}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{FinalPayload: []byte(`{}`)},
		},
		Lifecycle: &stubLifecycle{},
		Locks:     locks,
	}

	svc.runDispatchLoop(context.Background(), task, anyMeta("impl"), orchestrator.DefaultMachine())

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock released after awaiting transition")
	}
}

// TestBranchLock_ApplyAction_ReleasesOnAbort verifies that ApplyAction
// releases the lock on transitions out of executing (e.g. abort) even when
// the dispatch loop hasn't had a chance to release it yet.
func TestBranchLock_ApplyAction_ReleasesOnAbort(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	// Pre-acquire the lock to simulate a task that is mid-execution.
	if err := locks.AcquireForTask(context.Background(), "proj-1", "main", "task-1"); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	if !locks.IsHeldForTask("task-1") {
		t.Fatal("lock should be held after pre-acquire")
	}

	task := &orchestrator.Task{
		ID:         "task-1",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: anyMeta("impl")},
		Locks: locks,
	}

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "abort"}); err != nil {
		t.Fatalf("abort: %v", err)
	}

	if locks.IsHeldForTask("task-1") {
		t.Fatal("expected lock released after abort moved task out of executing")
	}
}

// TestBranchLock_ApplyAction_ReleasesOnAsk verifies that ask (executing →
// awaiting) releases the lock so other tasks can run on the same branch.
func TestBranchLock_ApplyAction_ReleasesOnAsk(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	if err := locks.AcquireForTask(context.Background(), "proj-1", "main", "task-1"); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}

	task := &orchestrator.Task{
		ID:         "task-1",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: anyMeta("impl")},
		Locks: locks,
	}

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "ask"}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if locks.IsHeldForTask("task-1") {
		t.Fatal("expected lock released after ask moved task to awaiting")
	}
}

// TestBranchLock_RunDispatchLoop_ReleasesOnDispatchError verifies that when
// DispatchAndAdvance returns an error, the branch lock is released and the
// task is transitioned to aborted (not left stuck in executing).
func TestBranchLock_RunDispatchLoop_ReleasesOnDispatchError(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "task-fail",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
	}

	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: &errorDispatchCoordinator{err: errors.New("worktree creation failed")},
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	svc.runDispatchLoop(context.Background(), task, anyMeta("impl"), orchestrator.DefaultMachine())

	if locks.IsHeldForTask(task.ID) {
		t.Fatal("expected lock released after dispatch error")
	}
	if txStore.updatedTask == nil {
		t.Fatal("expected task to be updated in DB after dispatch error")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected task status aborted after dispatch error, got %q", txStore.updatedTask.Status)
	}
}

// TestBranchLock_DispatchError_UnblocksSubsequentTask verifies the key scenario
// from the bug report: when task A fails with dispatch_error, task B (same
// base_branch) is unblocked and can proceed to dispatch.
func TestBranchLock_DispatchError_UnblocksSubsequentTask(t *testing.T) {
	locks := orchestrator.NewBranchLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	taskA := &orchestrator.Task{
		ID:         "task-a",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
	}
	txStoreA := &recordingTxStore{task: taskA}
	svcA := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStoreA},
		Coordinator: &errorDispatchCoordinator{err: errors.New("worktree creation failed")},
		Lifecycle:   &stubLifecycle{},
		Locks:       locks,
	}

	taskB := &orchestrator.Task{
		ID:         "task-b",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		Status:     orchestrator.TaskStatusExecuting,
		Behavior:   "impl",
		Payload:    []byte(`{}`),
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

	// Start task B in background — it will block waiting for the lock task A holds.
	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		svcB.runDispatchLoop(context.Background(), taskB, anyMeta("impl"), orchestrator.DefaultMachine())
	}()

	// Task A fails with dispatch error while holding the lock.
	svcA.runDispatchLoop(context.Background(), taskA, anyMeta("impl"), orchestrator.DefaultMachine())

	if locks.IsHeldForTask(taskA.ID) {
		t.Fatal("task A: expected lock released after dispatch error")
	}
	if txStoreA.updatedTask == nil || txStoreA.updatedTask.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("task A: expected aborted in DB, got %v", txStoreA.updatedTask)
	}

	// Task B should now be able to enter dispatch.
	select {
	case <-holdingB.enter:
		// expected
	case <-time.After(time.Second):
		t.Fatal("task B never entered dispatch after task A released the lock via dispatch error")
	}
	close(holdingB.release)
	<-doneB
}
