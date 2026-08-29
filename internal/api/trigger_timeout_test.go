package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// A trigger with no timeout is unbounded — the behavior every trigger written
// before the field existed must keep. An in-flight round stays a Skip forever.
func TestSweepTriggers_NoTimeout_RunStaysInFlight(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Run: "true"}}},
	})
	// A REAL running job row: without it reconcileInFlight self-heals the run
	// after its 3-minute grace, and the test would be measuring that instead.
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	result, err := svc.SweepTriggers(context.Background(), t0.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Completed) != 0 {
		t.Fatalf("Completed = %+v, want empty (no timeout declared)", result.Completed)
	}
	if len(result.Skipped) != 1 {
		t.Fatalf("Skipped = %+v, want 1", result.Skipped)
	}
}

// Below the bound, an in-flight round is an ordinary skip — the timeout must
// not shorten what a healthy long round is allowed to be.
func TestSweepTriggers_WithinTimeout_IsStillJustASkip(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// 8 minutes is a perfectly healthy round for this trigger, and it is more
	// than 1.5 × every (3m) — the yardstick trackSkipStreak had to invent
	// before `timeout` existed. It must not be ended here.
	result, err := svc.SweepTriggers(context.Background(), t0.Add(8*time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Completed) != 0 {
		t.Fatalf("Completed = %+v, want empty (well within the declared timeout)", result.Completed)
	}
	if len(result.Skipped) != 1 {
		t.Fatalf("Skipped = %+v, want 1", result.Skipped)
	}
}

// Past the bound the round is ENDED, and it arrives as an ordinary non-zero
// completion — which is what turns "taking too long" from something
// trackSkipStreak guesses at into something trackFailStreak counts.
func TestSweepTriggers_PastTimeout_EndsTheRunAsAFailure(t *testing.T) {
	svc, jobs, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	result, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Still recorded as a Skip: the ending is asynchronous and can fail, and
	// while it has not succeeded the row stays in flight. Dropping it here
	// would make a round that cannot be ended appear in neither Skipped nor
	// Fired — wedged with no notification ever firing.
	if len(result.Skipped) != 1 {
		t.Fatalf("Skipped = %+v, want 1 (the round is still in flight until the ending lands)", result.Skipped)
	}
	if result.Skipped[0].Timeout != 30*time.Minute {
		t.Errorf("Skipped[0].Timeout = %v, want 30m — trackSkipStreak sizes its threshold from this", result.Skipped[0].Timeout)
	}
	// The completion is recorded off the sweep goroutine (the abort and the job
	// stop can both block on the container engine, and runOnce is a single
	// goroutine covering every project), so it arrives on the NEXT sweep.
	if len(result.Completed) != 0 {
		t.Fatalf("Completed = %+v, want empty in the tick that started the ending", result.Completed)
	}
	drained := waitForTimeoutCompletion(t, svc)
	if len(drained) != 1 || drained[0].ExitCode != TriggerTimeoutExitCode {
		t.Fatalf("stashed completion = %+v, want exactly 1 with ExitCode=%d", drained, TriggerTimeoutExitCode)
	}
	// NOT re-fired in the same tick. Unlike the self-heal path (which closes a
	// row whose job was never really there and fires immediately), this tick
	// just ended a live round abnormally — deciding the next one on state we
	// mutated a few lines ago is worth avoiding for the one sweep interval it
	// costs. The next tick re-evaluates from scratch, `on: signals` predicate
	// included.
	if len(result.Fired) != 0 {
		t.Fatalf("Fired = %+v, want empty in the tick that ended the round", result.Fired)
	}
	if exec.callCount() != 0 {
		t.Fatalf("StartExec calls = %d, want 0", exec.callCount())
	}

	// ...and the next tick does start a fresh round, so ending the overrun
	// recovers rather than wedging the trigger. (waitForTimeoutCompletion
	// above already drained the stashed completion, so it does not reappear
	// here — a real tick would report it.)
	next, err := svc.SweepTriggers(context.Background(), t0.Add(32*time.Minute))
	if err != nil {
		t.Fatalf("next sweep: %v", err)
	}
	if len(next.Fired) != 1 {
		t.Fatalf("next tick Fired = %+v, want 1", next.Fired)
	}
}

// The load-bearing half: ending a round must end the TASK, not only the job.
// Stopping just the launcher leaves the work running, frees single-flight, and
// lets the next tick start a second concurrent round of the same work — worse
// than the overrun it was meant to cure.
func TestSweepTriggers_PastTimeout_AbortsTheTaskTheRoundWasWaitingOn(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	tx := &stubTx{}
	svc.Tasks = &stubTaskStore{task: &orchestrator.Task{
		ID:        "round-task",
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "sweep"},
	}}
	svc.Tx = tx
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()
	svc.TaskWaits.Register("job-1", "round-task")

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	waitForTimeoutCompletion(t, svc)

	if tx.createdAction == nil {
		t.Fatal("no action was recorded — the task the round was parked on was not ended")
	}
	if tx.createdAction.TaskID != "round-task" {
		t.Errorf("aborted task = %q, want round-task", tx.createdAction.TaskID)
	}
	if tx.createdAction.Type != "abort" {
		t.Errorf("action = %q, want abort", tx.createdAction.Type)
	}
	if tx.createdAction.ToStatus != orchestrator.TaskStatusAborted {
		t.Errorf("to_status = %q, want aborted", tx.createdAction.ToStatus)
	}
	if !strings.Contains(string(tx.createdAction.Payload), "trigger_timeout") {
		t.Errorf("payload = %s, want the trigger_timeout code so lifecycle.abort.code reads it", tx.createdAction.Payload)
	}
}

// An ordinary trigger whose `run:` does the work inline registers no wait —
// there is no task to end, and the round must still be closed rather than
// wedging on the missing attribution.
func TestSweepTriggers_PastTimeout_NoRegisteredWaitStillEndsTheRun(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	tx := &stubTx{}
	svc.Tx = tx
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	drained := waitForTimeoutCompletion(t, svc)
	if tx.createdAction != nil {
		t.Errorf("action = %+v, want none — nothing was registered to end", tx.createdAction)
	}
	if len(drained) != 1 || drained[0].ExitCode != TriggerTimeoutExitCode {
		t.Fatalf("stashed completion = %+v, want the run closed anyway", drained)
	}
}

func TestTaskWaitRegistry(t *testing.T) {
	reg := NewTaskWaitRegistry()

	if _, ok := reg.TaskFor("job-1"); ok {
		t.Error("an unregistered job must not resolve")
	}

	release := reg.Register("job-1", "task-a")
	if got, ok := reg.TaskFor("job-1"); !ok || got != "task-a" {
		t.Errorf("TaskFor = %q,%v, want task-a,true", got, ok)
	}
	release()
	if _, ok := reg.TaskFor("job-1"); ok {
		t.Error("the entry must be gone after release")
	}

	release()
}

// A stale release must not evict a LATER registration for the same job. This is
// reachable: a `run:` string can background two waits
// (`boid task wait A & boid task wait B & wait`), each shim invocation opens its
// own broker connection handled on its own goroutine, and they share a job id.
// If the first one's release could drop the second one's entry, only one of the
// two tasks would be abortable on timeout.
//
// The previous version of this test called the SAME release twice, which the
// sync.Once guard short-circuits before it ever reaches the ownership check —
// deleting that check left the suite green.
func TestTaskWaitRegistry_StaleReleaseKeepsTheNewerRegistration(t *testing.T) {
	reg := NewTaskWaitRegistry()

	releaseA := reg.Register("job-1", "task-a")
	releaseB := reg.Register("job-1", "task-b") // overwrites
	releaseA()                                  // stale: task-a is no longer the entry

	if got, ok := reg.TaskFor("job-1"); !ok || got != "task-b" {
		t.Fatalf("TaskFor = %q,%v, want task-b,true — a stale release evicted the newer wait", got, ok)
	}
	releaseB()
	if _, ok := reg.TaskFor("job-1"); ok {
		t.Error("the owner's release must still clear the entry")
	}
}

// A nil registry and an empty job id are both tolerated — a host-side or test
// caller has no job to attribute the wait to, and must not have to branch.
func TestTaskWaitRegistry_NilAndEmptyAreNoOps(t *testing.T) {
	var reg *TaskWaitRegistry
	reg.Register("job-1", "task-a")() // must not panic
	if _, ok := reg.TaskFor("job-1"); ok {
		t.Error("a nil registry must resolve nothing")
	}

	real := NewTaskWaitRegistry()
	real.Register("", "task-a")()
	if _, ok := real.TaskFor(""); ok {
		t.Error("an empty job id must not be registered")
	}
}

// The order timeOutTriggerRun's doc calls load-bearing: the TASK is ended before
// the JOB is stopped. Reversed, stopping the job first races the executor's
// deferred registry release, and the abort can be lost entirely — precisely the
// outcome the registry exists to prevent. Asserting only that both happened
// leaves that swap green.
func TestSweepTriggers_PastTimeout_AbortsBeforeStoppingTheJob(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	order := &orderRecordingLifecycle{}
	tx := &orderRecordingTx{order: order}
	svc.Tasks = &stubTaskStore{task: &orchestrator.Task{
		ID:        "round-task",
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "sweep"},
	}}
	svc.Tx = tx
	svc.Lifecycle = order
	svc.TaskWaits = NewTaskWaitRegistry()
	svc.TaskWaits.Register("job-1", "round-task")

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	waitForTimeoutCompletion(t, svc)

	got := order.steps()
	abortAt, stopAt := -1, -1
	for i, step := range got {
		if step == "abort" && abortAt < 0 {
			abortAt = i
		}
		if step == "stop:rt-1" && stopAt < 0 {
			stopAt = i
		}
	}
	if abortAt < 0 {
		t.Fatalf("steps = %v, want an abort", got)
	}
	if stopAt < 0 {
		t.Fatalf("steps = %v, want the job stopped", got)
	}
	if abortAt > stopAt {
		t.Errorf("steps = %v, want the abort BEFORE the job stop", got)
	}
}

// orderRecordingLifecycle / orderRecordingTx share one ordered log so a test can
// assert the RELATIVE order of the abort write and the job stop, not just that
// both happened. Both are touched from the goroutine beginTimeOutTriggerRun
// spawns, so the log is mutex-guarded.
type orderRecordingLifecycle struct {
	mu     sync.Mutex
	steps_ []string
}

func (l *orderRecordingLifecycle) record(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps_ = append(l.steps_, step)
}

func (l *orderRecordingLifecycle) steps() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps_...)
}

func (l *orderRecordingLifecycle) CompleteJob(string, JobCompletion)       {}
func (l *orderRecordingLifecycle) UnregisterJob(string)                    {}
func (l *orderRecordingLifecycle) CleanupTaskWindow(string)                {}
func (l *orderRecordingLifecycle) StopJobRuntime(runtimeID string)         { l.record("stop:" + runtimeID) }
func (l *orderRecordingLifecycle) SignalJobRuntime(string, syscall.Signal) {}

type orderRecordingTx struct {
	stubTx
	order *orderRecordingLifecycle
}

// WithinTx must hand the callback THIS wrapper, not the embedded stubTx —
// stubTx.WithinTx passes itself, which would route CreateAction straight past
// the override below and leave the ordering unobservable.
func (s *orderRecordingTx) WithinTx(fn func(TxStore) error) error { return fn(s) }

func (s *orderRecordingTx) CreateAction(ctx context.Context, action *orchestrator.Action) error {
	if action != nil && action.Type == "abort" {
		s.order.record("abort")
	}
	return s.stubTx.CreateAction(ctx, action)
}

// waitForTimeoutCompletion blocks until the goroutine beginTimeOutTriggerRun
// spawned has finished, which it signals by parking a completion for the next
// sweep to drain. Polled rather than slept on: the work is a few in-memory
// writes, so it lands almost immediately, and a fixed sleep would be both
// slower and flakier.
func waitForTimeoutCompletion(t *testing.T, svc *TaskWorkflowService) []TriggerCompletionResult {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if drained := svc.drainTimeoutCompletions(); len(drained) > 0 {
			return drained
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the timeout goroutine never recorded a completion")
	return nil
}

// A TRANSIENT abort failure must leave the run in flight, so single-flight
// stays held and the next tick tries again. Closing the run here would release
// the slot for a task that is still running — the second-concurrent-round
// failure this whole path exists to prevent.
func TestSweepTriggers_PastTimeout_TransientAbortFailureKeepsTheRunInFlight(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	// A transient failure inside the transaction. ApplyAction reports it as a
	// 500, which isPermanentAbortRefusal treats as retryable — only a 4xx
	// ("this task will never accept an abort") ends the round without the
	// abort.
	svc.Tasks = &stubTaskStore{task: &orchestrator.Task{
		ID:        "round-task",
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "sweep"},
	}}
	svc.Tx = &failingTx{err: errors.New("database is locked")}
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()
	svc.TaskWaits.Register("job-1", "round-task")

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	waitForTimeoutGoroutine(t, svc, run.ID)
	if drained := svc.drainTimeoutCompletions(); len(drained) != 0 {
		t.Fatalf("stashed completion = %+v, want none — a transient abort failure must not close the run", drained)
	}

	// The run is still in flight, so the next tick retries rather than firing a
	// fresh round against work that was never ended.
	next, err := svc.SweepTriggers(context.Background(), t0.Add(32*time.Minute))
	if err != nil {
		t.Fatalf("next sweep: %v", err)
	}
	if len(next.Fired) != 0 {
		t.Errorf("next tick Fired = %+v, want empty — the slot must still be held", next.Fired)
	}
}

// failingTx fails the transaction body itself, the way a busy database does.
// ApplyAction wraps that as a 500, which isPermanentAbortRefusal reads as
// transient — distinct from a failing STORE, which now also 500s (a read
// failure is not a missing row) but was the case that made the two
// indistinguishable before ApplyAction's error mapping was tightened.
type failingTx struct {
	stubTx
	err error
}

func (s *failingTx) WithinTx(fn func(TxStore) error) error {
	if err := fn(s); err != nil {
		return err
	}
	return s.err
}

// A task that refuses abort PERMANENTLY (a card, whose machine has no `abort`
// rule, so ApplyAction 400s every time) must not wedge the trigger: the round
// is ended without it. Retrying forever would leave single-flight held and the
// trigger silently dead.
func TestSweepTriggers_PastTimeout_PermanentAbortRefusalStillEndsTheRun(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	// A card: the card machine registers no `abort` rule, so ApplyAction
	// rejects it with a 400 that will never become a success.
	svc.Tasks = &stubTaskStore{task: &orchestrator.Task{
		ID:        "a-card",
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeCard,
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{},
	}}
	svc.Tx = &stubTx{}
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()
	svc.TaskWaits.Register("job-1", "a-card")

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	drained := waitForTimeoutCompletion(t, svc)
	if len(drained) != 1 || drained[0].ExitCode != TriggerTimeoutExitCode {
		t.Fatalf("stashed completion = %+v, want the run ended despite the refusal", drained)
	}
}

// A round whose ending keeps failing must stay visible to the stuck detector.
// Without the Skip record it appears in neither Skipped nor Fired, and both
// notification paths go quiet while the trigger is wedged — the same gap
// SweepTriggers' own comment closes for on:signals.
func TestSweepTriggers_PastTimeout_EndingThatKeepsFailingStaysSkipped(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	svc.Tasks = &stubTaskStore{task: &orchestrator.Task{
		ID:        "round-task",
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "sweep"},
	}}
	svc.Tx = &failingTx{err: errors.New("database is locked")}
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()
	svc.TaskWaits.Register("job-1", "round-task")

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	for _, at := range []time.Duration{31 * time.Minute, 32 * time.Minute} {
		result, err := svc.SweepTriggers(context.Background(), t0.Add(at))
		if err != nil {
			t.Fatalf("sweep at +%v: %v", at, err)
		}
		if len(result.Skipped) != 1 {
			t.Fatalf("sweep at +%v: Skipped = %+v, want 1 — a wedged ending must stay visible", at, result.Skipped)
		}
		if len(result.Fired) != 0 {
			t.Fatalf("sweep at +%v: Fired = %+v, want empty — the slot is still held", at, result.Fired)
		}
	}
}

// A failing task STORE must be transient, not permanent. ApplyAction used to
// map every GetTask error to 404, which made a busy/locked read
// indistinguishable from a deleted row — and isPermanentAbortRefusal reads a
// 4xx as "this task will never accept an abort", so one unlucky read at the
// moment a round times out would skip the abort, kill the launcher, close the
// run, release single-flight, and leave the task running for the next tick to
// start a second round alongside. orchestrator.GetTask returns the
// ErrTaskNotFound sentinel for a genuinely missing row, so the two ARE
// distinguishable; this pins that they are distinguished.
func TestSweepTriggers_PastTimeout_FailingTaskReadIsTransientNotPermanent(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	// A read failure, NOT ErrTaskNotFound.
	svc.Tasks = &stubTaskStore{err: errors.New("database is locked")}
	svc.Tx = &stubTx{}
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()
	svc.TaskWaits.Register("job-1", "round-task")

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	waitForTimeoutGoroutine(t, svc, run.ID)
	if drained := svc.drainTimeoutCompletions(); len(drained) != 0 {
		t.Fatalf("stashed completion = %+v, want none — a failed READ is not a task refusing abort", drained)
	}

	// The slot is still held, so no second round starts against work that was
	// never ended.
	next, err := svc.SweepTriggers(context.Background(), t0.Add(32*time.Minute))
	if err != nil {
		t.Fatalf("next sweep: %v", err)
	}
	if len(next.Fired) != 0 {
		t.Errorf("next tick Fired = %+v, want empty", next.Fired)
	}
}

// A genuinely missing task (the sentinel) IS permanent — there is nothing to
// abort, so the round must be ended rather than retried forever.
func TestSweepTriggers_PastTimeout_MissingTaskIsPermanent(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	svc.Tasks = &stubTaskStore{err: orchestrator.ErrTaskNotFound}
	svc.Tx = &stubTx{}
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()
	svc.TaskWaits.Register("job-1", "gone-task")

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	drained := waitForTimeoutCompletion(t, svc)
	if len(drained) != 1 || drained[0].ExitCode != TriggerTimeoutExitCode {
		t.Fatalf("stashed completion = %+v, want the run ended (nothing to abort)", drained)
	}
}

// The timeout reason must land ONLY on the abort action, never merged into the
// task's own payload. ApplyAction's generic merge writes a non-side-effect
// action's payload into `task.Exec.Payload`, so a `{code, message}` abort
// reason would silently replace an existing top-level `message` there — an
// ordinary key for signal-derived work, and visible afterwards in
// `boid task show --field payload`, the Web UI, and `boid task payload` if the
// task is later reopened (aborted → executing is a first-class edge).
//
// abortOnDispatchError, the other daemon-initiated abort, avoids this by
// writing the action directly; this path keeps ApplyAction for its legality
// check and opts out of the merge instead.
func TestSweepTriggers_PastTimeout_AbortReasonDoesNotOverwriteTheTaskPayload(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	tx := &stubTx{}
	svc.Tasks = &stubTaskStore{task: &orchestrator.Task{
		ID:        "round-task",
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{
			Behavior: "sweep",
			Payload:  []byte(`{"message":"the real work item","other":1}`),
		},
	}}
	svc.Tx = tx
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()
	svc.TaskWaits.Register("job-1", "round-task")

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	waitForTimeoutCompletion(t, svc)

	// The reason IS on the action — that is what lifecycle.abort.code reads.
	if tx.createdAction == nil || !strings.Contains(string(tx.createdAction.Payload), "trigger_timeout") {
		t.Fatalf("action payload = %v, want the trigger_timeout reason", tx.createdAction)
	}
	// ...and NOT in the task's payload.
	if tx.updatedTask == nil || tx.updatedTask.Exec == nil {
		t.Fatal("no task update recorded")
	}
	var payload map[string]any
	if err := json.Unmarshal(tx.updatedTask.Exec.Payload, &payload); err != nil {
		t.Fatalf("unmarshal task payload: %v", err)
	}
	if got := payload["message"]; got != "the real work item" {
		t.Errorf("task payload message = %v, want it untouched — the abort reason overwrote it", got)
	}
	if _, leaked := payload["code"]; leaked {
		t.Errorf("task payload = %v, want no `code` key — the abort reason leaked into the task", payload)
	}
}

// waitForTimeoutGoroutine blocks until the goroutine holding runID has
// released it, and FAILS if it never does. The earlier version of this wait
// just broke out of its loop on the deadline, so a slow goroutine turned the
// "nothing was stashed" assertion that follows into a vacuous pass.
func waitForTimeoutGoroutine(t *testing.T, svc *TaskWorkflowService, runID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, held := svc.timeoutHeld(runID); !held {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the timeout goroutine never released the run — the assertions after this would be vacuous")
}

// The loser of the close race must NOT report a completion. Two closers race
// for the same row: reconcileInFlight (the job reached a terminal status,
// which the abort itself causes) and this goroutine. The store guard makes the
// second write fail; this pins the loop's half — that the goroutine then stays
// quiet, instead of stashing a second completion and counting one round twice
// against the fail streak.
func TestSweepTriggers_PastTimeout_LosingTheCloseRaceReportsNothing(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	svc.Tx = &stubTx{}
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// The other closer got there first — exactly what reconcileInFlight does
	// when it sees the job terminal while this goroutine is still working.
	if err := svc.Triggers.CompleteTriggerRun(run.ID, t0.Add(time.Minute), 1); err != nil {
		t.Fatalf("seed the winning close: %v", err)
	}

	// Drive the timeout path directly: with the row already closed it is no
	// longer in flight, so a full sweep would not reach the branch at all.
	svc.beginTimeOutTriggerRun(context.Background(), TriggerKey{ProjectID: "proj-1", TriggerName: "sweep"},
		run, 30*time.Minute)
	waitForTimeoutGoroutine(t, svc, run.ID)

	if drained := svc.drainTimeoutCompletions(); len(drained) != 0 {
		t.Fatalf("stashed completion = %+v, want none — the loser must not report the round a second time", drained)
	}
}

// Draining empties pendingTimeouts, so a drain followed by an error return
// destroys those completions — runOnce discards the result on error and never
// reaches trackFailStreak. The tick where that matters is the likely one: a
// database busy enough to fail the reconcile pass is busy enough for a round to
// have timed out. Pinned by making the sweep fail and asserting the stashed
// completion survives for the next tick.
func TestSweepTriggers_StashedCompletionSurvivesASweepError(t *testing.T) {
	svc, _, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	svc.stashTimeoutCompletion(TriggerCompletionResult{
		TriggerKey: TriggerKey{ProjectID: "proj-1", TriggerName: "sweep"},
		ExitCode:   TriggerTimeoutExitCode,
	})
	svc.Projects = failingProjectRepository{ProjectRepository: svc.Projects}

	if _, err := svc.SweepTriggers(context.Background(), time.Now()); err == nil {
		t.Fatal("expected the sweep to fail")
	}
	if drained := svc.drainTimeoutCompletions(); len(drained) != 1 {
		t.Fatalf("stashed completions after a failed sweep = %+v, want the entry still there", drained)
	}
}

type failingProjectRepository struct {
	ProjectRepository
}

func (failingProjectRepository) ListProjects() ([]*orchestrator.Project, error) {
	return nil, errors.New("database is locked")
}

// When the timeout path wins the close race, the reconcile pass must not report
// the run as still in flight. Treating the sentinel as a failure would mark the
// trigger busy for a tick it is not, and hand trackSkipStreak a Skip for a
// round that already ended.
//
// The race is a TOCTOU — the row IS listed as in flight when reconcile reads it
// and is closed by the goroutine before the update lands — so it has to be
// staged with a store whose list and close disagree; seeding a closed row
// instead would simply drop it from the in-flight list and never reach the
// branch at all.
func TestSweepTriggers_ReconcileLosingTheCloseRaceDoesNotHoldTheSlot(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Run: "true"}}},
	})
	// The job has exited, so reconcileInFlight tries to close the row...
	jobs.set(&Job{ID: "job-1", Status: JobStatusCompleted, ExitCode: 1})
	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// ...but the timeout goroutine closed it between the list and the update.
	svc.Triggers = failingCloseTriggerStore{
		TriggerRunStore: svc.Triggers,
		err:             orchestrator.ErrTriggerRunAlreadyFinished,
	}

	result, err := svc.SweepTriggers(context.Background(), t0.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Completed) != 0 {
		t.Errorf("Completed = %+v, want empty — the winner already reported it", result.Completed)
	}
	// The loser must not report the run as busy. Distinguished by `Since`:
	// reconcile holding the slot yields a Skip dated from when the RUN started,
	// while the Skip this fixture legitimately produces is dated NOW, from the
	// create race the stub causes (it fakes the close, so the real row is still
	// open and the single-flight UNIQUE index rejects a new create — an
	// artifact of staging a TOCTOU, not product behavior).
	for _, skip := range result.Skipped {
		if skip.Since.Equal(t0) {
			t.Errorf("Skipped = %+v — reconcile reported the run busy since it started, though its row is closed", skip)
		}
	}
}

// A CompleteTriggerRun failure that is NOT a lost race must also not stash: the
// row stays in flight, the next tick retries, and reporting a completion we
// failed to persist would let a second round start against a row that is still
// claimed. The lost-race sibling is pinned above; this is the other branch.
func TestSweepTriggers_PastTimeout_CloseFailureReportsNothing(t *testing.T) {
	svc, jobs, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "2m", Timeout: "30m", Run: "true"}}},
	})
	jobs.set(&Job{ID: "job-1", Status: JobStatusRunning, RuntimeID: "rt-1"})
	svc.Tx = &stubTx{}
	svc.Lifecycle = &stubLifecycle{}
	svc.TaskWaits = NewTaskWaitRegistry()

	t0 := time.Now()
	run := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "sweep", JobID: "job-1", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	svc.Triggers = failingCloseTriggerStore{TriggerRunStore: svc.Triggers, err: errors.New("database is locked")}

	svc.beginTimeOutTriggerRun(context.Background(), TriggerKey{ProjectID: "proj-1", TriggerName: "sweep"},
		run, 30*time.Minute)
	waitForTimeoutGoroutine(t, svc, run.ID)

	if drained := svc.drainTimeoutCompletions(); len(drained) != 0 {
		t.Fatalf("stashed completion = %+v, want none — the close never landed", drained)
	}
}

type failingCloseTriggerStore struct {
	TriggerRunStore
	err error
}

func (s failingCloseTriggerStore) CompleteTriggerRun(id string, finishedAt time.Time, exitCode int) error {
	return s.err
}
