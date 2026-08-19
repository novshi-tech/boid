package api

// docs/plans/ingestion-identity.md PR-4 (B-5): SweepTriggers / RunTriggerNow
// / TriggerLoop の「検証」節を直接 pin する:
//   - every の経過でちょうど 1 回起きる
//   - 前回が走っている間は見送る (single-flight) — 同じ (project, trigger)
//     が同時に 2 つ走らない一方、別の (project, trigger) は独立に走る
//   - コマンドが非ゼロで落ちても次の巡が回り、exit code が記録に残る
//   - 実行 dispatch 自体が失敗してもフェイルオープン (次の巡が回る)
//   - readonly trigger job (この PR の前提そのものの一部) — StartExecRequest
//     の Readonly が実際に true で飛ぶことをここで、実際に op が通ることは
//     internal/server/trigger_readonly_task_create_test.go で pin する
//
// internal/api のテストは testutil を import すると internal/server 経由の
// 循環 import になる (task_resolve_or_capture_test.go と同じ理由) ので、
// db.Open(":memory:") + migrate.Apply の直接呼び出しパターンを使う。

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// fakeTriggerExecDispatcher stands in for api.ExecDispatcher — records every
// StartExec call and hands back a fresh job id each time, auto-registering a
// "running" row in jobs so GetJob succeeds immediately after dispatch. A
// test flips a job to terminal via jobs.complete(id, exitCode).
type fakeTriggerExecDispatcher struct {
	jobs      *fakeTriggerJobStore
	nextJobID int
	// failNext, when > 0, makes the next N StartExec calls fail instead of
	// dispatching — for pinning the fail-open path.
	failNext int
	calls    []StartExecRequest
}

func (f *fakeTriggerExecDispatcher) StartExec(_ context.Context, req StartExecRequest) (*StartExecResult, error) {
	f.calls = append(f.calls, req)
	if f.failNext > 0 {
		f.failNext--
		return nil, errors.New("forced exec dispatch failure")
	}
	f.nextJobID++
	id := fmt.Sprintf("job-%d", f.nextJobID)
	f.jobs.set(&Job{ID: id, Status: JobStatusRunning})
	return &StartExecResult{JobID: id, AttachURL: "/jobs/" + id}, nil
}

// fakeTriggerJobStore is a minimal in-memory JobStore keyed by ID.
type fakeTriggerJobStore struct {
	byID map[string]*Job
}

func newFakeTriggerJobStore() *fakeTriggerJobStore {
	return &fakeTriggerJobStore{byID: map[string]*Job{}}
}

func (f *fakeTriggerJobStore) set(job *Job) { f.byID[job.ID] = job }

// complete flips a job to a terminal status — simulates the sandbox exiting.
func (f *fakeTriggerJobStore) complete(id string, exitCode int) {
	if j, ok := f.byID[id]; ok {
		j.Status = JobStatusCompleted
		j.ExitCode = exitCode
	}
}

func (f *fakeTriggerJobStore) GetJob(id string) (*Job, error) {
	if j, ok := f.byID[id]; ok {
		return j, nil
	}
	return nil, fmt.Errorf("job not found: %s", id)
}
func (f *fakeTriggerJobStore) ListJobsByTask(_ string) ([]*Job, error) { return nil, nil }
func (f *fakeTriggerJobStore) UpdateJob(job *Job) error                { f.byID[job.ID] = job; return nil }

// fakeTriggerMetaStore is a multi-project-keyed MetaStore (unlike
// stubMetaStore in service_test.go, which always returns the same single
// meta regardless of project id — SweepTriggers needs different Triggers
// per project in the same test).
type fakeTriggerMetaStore struct {
	byProject map[string]*orchestrator.ProjectMeta
}

func (f fakeTriggerMetaStore) Get(id string) (*orchestrator.ProjectMeta, bool) {
	m, ok := f.byProject[id]
	return m, ok
}
func (f fakeTriggerMetaStore) GetWithWorkspace(_ context.Context, id string) (*orchestrator.ProjectMeta, error) {
	m, ok := f.byProject[id]
	if !ok {
		return nil, fmt.Errorf("fakeTriggerMetaStore: no meta for %q", id)
	}
	return m, nil
}

// newTriggerSweepTestService builds a TaskWorkflowService wired for
// SweepTriggers/RunTriggerNow tests: real orchestrator.TaskRepository
// (Triggers) and orchestrator.ProjectRepository (Projects) over a real
// sqlite :memory: DB, plus the fakes above for Meta/Jobs/Exec.
func newTriggerSweepTestService(t *testing.T, projects map[string]*orchestrator.ProjectMeta) (*TaskWorkflowService, *fakeTriggerJobStore, *fakeTriggerExecDispatcher) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for id := range projects {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
	}

	repo := orchestrator.NewTaskRepository(d.Conn)
	jobs := newFakeTriggerJobStore()
	exec := &fakeTriggerExecDispatcher{jobs: jobs}

	svc := &TaskWorkflowService{
		Triggers: repo,
		Projects: orchestrator.NewProjectRepository(d.Conn),
		Meta:     fakeTriggerMetaStore{byProject: projects},
		Jobs:     jobs,
		Exec:     exec,
	}
	return svc, jobs, exec
}

func TestSweepTriggers_NeverRunBefore_FiresExactlyOnce(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "python3 tick.py"}}},
	})
	now := time.Now()

	result, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepTriggers: %v", err)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("Fired = %+v, want exactly 1", result.Fired)
	}
	if result.Fired[0].ProjectID != "proj-1" || result.Fired[0].TriggerName != "intake" {
		t.Errorf("Fired[0] = %+v, unexpected", result.Fired[0])
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1", len(exec.calls))
	}
	got := exec.calls[0]
	if got.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want proj-1", got.ProjectID)
	}
	if len(got.Argv) != 3 || got.Argv[0] != "sh" || got.Argv[1] != "-c" || got.Argv[2] != "python3 tick.py" {
		t.Errorf("Argv = %v, want [sh -c \"python3 tick.py\"] (J-2: run は sh -c にそのまま渡すコマンド文字列)", got.Argv)
	}
	if !got.Readonly {
		t.Error("Readonly = false, want true (PR-4 節: Readonly true 固定)")
	}

	// A second sweep at the exact same instant must NOT fire again — the
	// job is still "running" (job store default), so single-flight blocks
	// it (この PR の不変条件: 同じ (project, trigger) が同時に 2 つ走らない).
	result2, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("second SweepTriggers: %v", err)
	}
	if len(result2.Fired) != 0 {
		t.Errorf("second sweep Fired = %+v, want empty (single-flight)", result2.Fired)
	}
	if len(result2.Skipped) != 1 || result2.Skipped[0] != (TriggerKey{ProjectID: "proj-1", TriggerName: "intake"}) {
		t.Errorf("second sweep Skipped = %+v, want exactly [proj-1/intake]", result2.Skipped)
	}
	if len(exec.calls) != 1 {
		t.Errorf("StartExec calls after second sweep = %d, want still 1 (no second dispatch)", len(exec.calls))
	}
}

func TestSweepTriggers_EveryElapsed_FiresAgainOnlyAfterInterval(t *testing.T) {
	svc, jobs, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "true"}}},
	})
	now := time.Now()

	if _, err := svc.SweepTriggers(context.Background(), now); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	jobs.complete("job-1", 0)

	// Not yet elapsed (5m < 10m) — must not fire, and the completed run's
	// row must reconcile out of "in flight" without creating a new one.
	before := now.Add(5 * time.Minute)
	result, err := svc.SweepTriggers(context.Background(), before)
	if err != nil {
		t.Fatalf("sweep before elapsed: %v", err)
	}
	if len(result.Fired) != 0 {
		t.Errorf("Fired before every elapsed = %+v, want empty", result.Fired)
	}
	if len(result.Skipped) != 0 {
		t.Errorf("Skipped before every elapsed = %+v, want empty (the run already completed, not blocking)", result.Skipped)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls before elapsed = %d, want still 1", len(exec.calls))
	}

	// Elapsed (11m >= 10m) — fires exactly once more.
	after := now.Add(11 * time.Minute)
	result2, err := svc.SweepTriggers(context.Background(), after)
	if err != nil {
		t.Fatalf("sweep after elapsed: %v", err)
	}
	if len(result2.Fired) != 1 {
		t.Fatalf("Fired after every elapsed = %+v, want exactly 1", result2.Fired)
	}
	if len(exec.calls) != 2 {
		t.Errorf("StartExec calls after elapsed = %d, want 2", len(exec.calls))
	}
}

func TestSweepTriggers_NonZeroExitCode_RecordedAndNextTickRetries(t *testing.T) {
	svc, jobs, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "1m", Run: "false"}}},
	})
	now := time.Now()

	if _, err := svc.SweepTriggers(context.Background(), now); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	jobs.complete("job-1", 7)

	reconcileAt := now.Add(30 * time.Second)
	result, err := svc.SweepTriggers(context.Background(), reconcileAt)
	if err != nil {
		t.Fatalf("reconcile sweep: %v", err)
	}
	if len(result.Completed) != 1 {
		t.Fatalf("Completed = %+v, want exactly 1", result.Completed)
	}
	if result.Completed[0].ExitCode != 7 {
		t.Errorf("Completed[0].ExitCode = %d, want 7 (a failing trigger's exit code must be recorded, not swallowed)", result.Completed[0].ExitCode)
	}

	// Next tick (every elapsed) fires again despite the prior failure — a
	// failing command does not permanently block future runs, only an
	// IN-FLIGHT one does (single-flight, not failure, is what blocks).
	after := now.Add(90 * time.Second)
	result2, err := svc.SweepTriggers(context.Background(), after)
	if err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if len(result2.Fired) != 1 {
		t.Fatalf("Fired on retry = %+v, want exactly 1 (failure must not block future runs)", result2.Fired)
	}
	if len(exec.calls) != 2 {
		t.Errorf("StartExec calls = %d, want 2", len(exec.calls))
	}
}

func TestSweepTriggers_DispatchFailure_FailsOpenAndRetriesImmediately(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "true"}}},
	})
	exec.failNext = 1
	now := time.Now()

	result, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepTriggers should not return an error for a dispatch failure (logged, not surfaced): %v", err)
	}
	if len(result.Fired) != 0 {
		t.Errorf("Fired = %+v, want empty on dispatch failure", result.Fired)
	}
	if len(result.Skipped) != 0 {
		t.Errorf("Skipped = %+v, want empty — a dispatch failure is not a single-flight skip", result.Skipped)
	}

	// フェイルオープン: the SAME instant (no time elapsed at all) retries
	// successfully, because no trigger_runs row was ever created for the
	// failed attempt — triggerIsDue still sees ErrTriggerRunNotFound.
	result2, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if len(result2.Fired) != 1 {
		t.Fatalf("Fired on immediate retry = %+v, want exactly 1 (fail-open: next tick just retries)", result2.Fired)
	}
	if len(exec.calls) != 2 {
		t.Errorf("StartExec calls = %d, want 2 (one failed attempt + one successful retry)", len(exec.calls))
	}
}

// TestSweepTriggers_DifferentTriggerKeys_RunIndependently pins that
// single-flight's scope is the (project, trigger) PAIR, not the whole
// project or the whole daemon: an in-flight run for "intake" must not block
// a due "sweep" trigger in the SAME project, nor a trigger in a DIFFERENT
// project.
func TestSweepTriggers_DifferentTriggerKeys_RunIndependently(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{
			{Name: "intake", Every: "10m", Run: "true"},
			{Name: "sweep", Every: "10m", Run: "true"},
		}},
		"proj-2": {Triggers: []orchestrator.Trigger{
			{Name: "intake", Every: "10m", Run: "true"},
		}},
	})
	now := time.Now()

	// First sweep fires all three (all never-run-before).
	result, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(result.Fired) != 3 {
		t.Fatalf("Fired = %+v, want 3 (all independent, all due)", result.Fired)
	}
	if len(exec.calls) != 3 {
		t.Fatalf("StartExec calls = %d, want 3", len(exec.calls))
	}

	// Second sweep at the same instant: all three are still "running" (no
	// job completed) — every one of them must be independently skipped
	// (not just one, proving the per-key scoping, not a global lock).
	result2, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(result2.Skipped) != 3 {
		t.Fatalf("Skipped = %+v, want 3 (each (project,trigger) pair blocked independently)", result2.Skipped)
	}
}

func TestSweepTriggers_NilTriggersOrProjectsOrMeta_NoOp(t *testing.T) {
	svc := &TaskWorkflowService{}
	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers on an unwired service: %v", err)
	}
	if len(result.Fired) != 0 || len(result.Skipped) != 0 || len(result.Completed) != 0 {
		t.Errorf("result = %+v, want an entirely empty no-op", result)
	}
}

// ---- RunTriggerNow ----

func TestRunTriggerNow_FiresImmediately_IgnoringEveryElapsed(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "24h", Run: "true"}}},
	})

	result, err := svc.RunTriggerNow(context.Background(), "proj-1", "intake")
	if err != nil {
		t.Fatalf("RunTriggerNow: %v", err)
	}
	if result.Skipped {
		t.Fatalf("result = %+v, want Skipped=false (nothing in flight yet)", result)
	}
	if result.JobID == "" {
		t.Error("JobID is empty")
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1 (every=24h must not block a manual run)", len(exec.calls))
	}
}

func TestRunTriggerNow_SingleFlightBlocksASecondManualRun(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "1m", Run: "true"}}},
	})

	first, err := svc.RunTriggerNow(context.Background(), "proj-1", "intake")
	if err != nil {
		t.Fatalf("first RunTriggerNow: %v", err)
	}
	if first.Skipped {
		t.Fatal("first RunTriggerNow was skipped, want it to have fired")
	}

	second, err := svc.RunTriggerNow(context.Background(), "proj-1", "intake")
	if err != nil {
		t.Fatalf("second RunTriggerNow: %v", err)
	}
	if !second.Skipped {
		t.Fatal("second RunTriggerNow: Skipped = false, want true (single-flight — a manual run must not bypass it)")
	}
	if second.Reason == "" {
		t.Error("Reason is empty on a skip")
	}
	if len(exec.calls) != 1 {
		t.Errorf("StartExec calls = %d, want still 1 (no second concurrent dispatch)", len(exec.calls))
	}
}

func TestRunTriggerNow_UnknownTrigger_404(t *testing.T) {
	svc, _, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "1m", Run: "true"}}},
	})
	_, err := svc.RunTriggerNow(context.Background(), "proj-1", "does-not-exist")
	if err == nil {
		t.Fatal("RunTriggerNow for an unknown trigger name = nil error, want a rejection")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != 404 {
		t.Fatalf("error = %v, want a 404 StatusError", err)
	}
}

func TestRunTriggerNow_UnknownProject_404(t *testing.T) {
	svc, _, _ := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{})
	_, err := svc.RunTriggerNow(context.Background(), "no-such-project", "intake")
	if err == nil {
		t.Fatal("RunTriggerNow for an unknown project = nil error, want a rejection")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != 404 {
		t.Fatalf("error = %v, want a 404 StatusError", err)
	}
}

// ---- TriggerLoop (scheduler shell — QueueSweepLoop-shaped) ----

// fakeTriggerLoopStore is a canned-sequence TriggerLoopStore: each call to
// SweepTriggers pops the next result off a queue (or repeats the last one
// once the queue is drained), and counts calls.
type fakeTriggerLoopStore struct {
	results   []TriggerSweepResult
	callCount atomic.Int64
}

func (f *fakeTriggerLoopStore) SweepTriggers(_ context.Context, _ time.Time) (TriggerSweepResult, error) {
	n := f.callCount.Add(1)
	idx := int(n) - 1
	if idx >= len(f.results) {
		if len(f.results) == 0 {
			return TriggerSweepResult{}, nil
		}
		idx = len(f.results) - 1
	}
	return f.results[idx], nil
}

// fakeTriggerNotifier records every Notify call.
type fakeTriggerNotifier struct {
	messages []string
}

func (f *fakeTriggerNotifier) Notify(_ context.Context, ev notify.Event) error {
	f.messages = append(f.messages, ev.Message)
	return nil
}

func TestTriggerLoop_CallsSweepMultipleTimes(t *testing.T) {
	store := &fakeTriggerLoopStore{}
	loop := &TriggerLoop{Store: store, Interval: 10 * time.Millisecond, InitialDelay: 1 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	time.Sleep(150 * time.Millisecond)
	cancel()

	if got := store.callCount.Load(); got < 3 {
		t.Fatalf("SweepTriggers calls = %d, want at least 3", got)
	}
}

func TestTriggerLoop_CtxCancelExits(t *testing.T) {
	store := &fakeTriggerLoopStore{}
	loop := &TriggerLoop{Store: store, Interval: 10 * time.Millisecond, InitialDelay: 1 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("TriggerLoop.Run did not exit after ctx cancel")
	}
}

func TestTriggerLoop_SkipStreak_NotifiesEveryNthConsecutiveSkip(t *testing.T) {
	key := TriggerKey{ProjectID: "proj-1", TriggerName: "intake"}
	notifier := &fakeTriggerNotifier{}
	loop := &TriggerLoop{Notifier: notifier}

	for i := 0; i < TriggerStuckSkipThreshold-1; i++ {
		loop.trackSkipStreak(context.Background(), []TriggerKey{key})
	}
	if len(notifier.messages) != 0 {
		t.Fatalf("messages after %d skips = %v, want none yet (threshold is %d)", TriggerStuckSkipThreshold-1, notifier.messages, TriggerStuckSkipThreshold)
	}

	loop.trackSkipStreak(context.Background(), []TriggerKey{key}) // Nth skip
	if len(notifier.messages) != 1 {
		t.Fatalf("messages after %d skips = %v, want exactly 1", TriggerStuckSkipThreshold, notifier.messages)
	}

	// A tick where the key is NOT skipped (it fired, or resolved) resets
	// the streak — a subsequent run of N-1 skips must not notify again.
	loop.trackSkipStreak(context.Background(), nil)
	for i := 0; i < TriggerStuckSkipThreshold-1; i++ {
		loop.trackSkipStreak(context.Background(), []TriggerKey{key})
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("messages after reset + %d more skips = %v, want still exactly 1 (streak was reset)", TriggerStuckSkipThreshold-1, notifier.messages)
	}
}

func TestTriggerLoop_FailStreak_NotifiesAndResetsOnSuccess(t *testing.T) {
	key := TriggerKey{ProjectID: "proj-1", TriggerName: "intake"}
	notifier := &fakeTriggerNotifier{}
	loop := &TriggerLoop{Notifier: notifier}

	for i := 0; i < TriggerStuckFailStreakThreshold; i++ {
		loop.trackFailStreak(context.Background(), []TriggerCompletionResult{{TriggerKey: key, ExitCode: 1}})
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("messages after %d consecutive failures = %v, want exactly 1", TriggerStuckFailStreakThreshold, notifier.messages)
	}

	// A success resets the streak.
	loop.trackFailStreak(context.Background(), []TriggerCompletionResult{{TriggerKey: key, ExitCode: 0}})
	for i := 0; i < TriggerStuckFailStreakThreshold-1; i++ {
		loop.trackFailStreak(context.Background(), []TriggerCompletionResult{{TriggerKey: key, ExitCode: 1}})
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("messages after a success + %d more failures = %v, want still exactly 1 (streak was reset by the success)", TriggerStuckFailStreakThreshold-1, notifier.messages)
	}
}
