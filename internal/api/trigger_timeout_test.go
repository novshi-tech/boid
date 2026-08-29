package api

import (
	"context"
	"strings"
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
	if len(result.Completed) != 1 || result.Completed[0].ExitCode != TriggerTimeoutExitCode {
		t.Fatalf("Completed = %+v, want exactly 1 with ExitCode=%d", result.Completed, TriggerTimeoutExitCode)
	}
	if len(result.Skipped) != 0 {
		t.Fatalf("Skipped = %+v, want empty — an ended round is not a skip", result.Skipped)
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
	// recovers rather than wedging the trigger.
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

	result, err := svc.SweepTriggers(context.Background(), t0.Add(31*time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if tx.createdAction != nil {
		t.Errorf("action = %+v, want none — nothing was registered to end", tx.createdAction)
	}
	if len(result.Completed) != 1 || result.Completed[0].ExitCode != TriggerTimeoutExitCode {
		t.Fatalf("Completed = %+v, want the run closed anyway", result.Completed)
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

	// Releasing twice must not disturb a later registration for the same job.
	release2 := reg.Register("job-1", "task-b")
	release()
	if got, ok := reg.TaskFor("job-1"); !ok || got != "task-b" {
		t.Errorf("after a stale release, TaskFor = %q,%v, want task-b,true", got, ok)
	}
	release2()
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
