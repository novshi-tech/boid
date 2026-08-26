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
	"path/filepath"
	"sync"
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
//
// Guarded by a mutex (Opus review Blocker 1's concurrency tests call
// StartExec from multiple goroutines at once — go test -race must stay
// clean) and supports an artificial delay so a test can force a wide enough
// window for two concurrent callers to race (StartExec on the real
// container backend takes real wall-clock seconds; delay simulates that
// without an actual container).
type fakeTriggerExecDispatcher struct {
	mu        sync.Mutex
	jobs      *fakeTriggerJobStore
	nextJobID int
	// failNext, when > 0, makes the next N StartExec calls fail instead of
	// dispatching — for pinning the fail-open path.
	failNext int
	calls    []StartExecRequest
	// delay, when > 0, is slept BEFORE recording the call/dispatching a job
	// — simulating a container backend that takes real time to start, wide
	// enough that a concurrent caller's own DB read-then-decide can complete
	// well within it (Opus review Blocker 1's concurrency tests).
	delay time.Duration
}

func (f *fakeTriggerExecDispatcher) StartExec(_ context.Context, req StartExecRequest) (*StartExecResult, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeTriggerExecDispatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeTriggerJobStore is a minimal in-memory JobStore keyed by ID, guarded
// by a mutex so concurrent SweepTriggers/RunTriggerNow calls (Opus review
// Blocker 1's concurrency tests) can safely reconcile against it under
// go test -race.
type fakeTriggerJobStore struct {
	mu   sync.Mutex
	byID map[string]*Job
}

func newFakeTriggerJobStore() *fakeTriggerJobStore {
	return &fakeTriggerJobStore{byID: map[string]*Job{}}
}

func (f *fakeTriggerJobStore) set(job *Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[job.ID] = job
}

// complete flips a job to a terminal status — simulates the sandbox exiting.
func (f *fakeTriggerJobStore) complete(id string, exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.byID[id]; ok {
		j.Status = JobStatusCompleted
		j.ExitCode = exitCode
	}
}

func (f *fakeTriggerJobStore) GetJob(id string) (*Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.byID[id]; ok {
		// Copy out: the caller must not observe/mutate the stored *Job
		// concurrently with a later complete() call.
		cp := *j
		return &cp, nil
	}
	return nil, fmt.Errorf("job not found: %s", id)
}
func (f *fakeTriggerJobStore) ListJobsByTask(_ string) ([]*Job, error) { return nil, nil }
func (f *fakeTriggerJobStore) UpdateJob(job *Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[job.ID] = job
	return nil
}

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

// newTriggerSweepTestServiceOnFile is newTriggerSweepTestService's file-DB
// variant, for the "two daemon processes" concurrency test (Opus review
// Blocker 1): dbPath is opened as a FRESH db.Open connection (its own
// SetMaxOpenConns(1) pool, matching one real daemon process) — the caller is
// expected to open a SECOND independent service against the SAME dbPath to
// simulate two daemon processes sharing one sqlite file, with NOTHING
// shared between them at the Go level (no shared *sql.DB, no shared
// in-memory map) — only the DB file itself, which is exactly what the
// partial UNIQUE index (migration 0043) has to enforce single-flight
// against for this scenario to mean anything.
func newTriggerSweepTestServiceOnFile(t *testing.T, dbPath string, migrateSchema bool, projects map[string]*orchestrator.ProjectMeta) (*TaskWorkflowService, *fakeTriggerJobStore, *fakeTriggerExecDispatcher) {
	t.Helper()
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db %s: %v", dbPath, err)
	}
	t.Cleanup(func() { d.Close() })
	if migrateSchema {
		if err := migrate.Apply(d.Conn); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		for id := range projects {
			if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
				t.Fatalf("create project %s: %v", id, err)
			}
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
	// N-4 (Opus review): the command is wrapped with a stdin-closing prefix
	// (triggerRunArgv) — verified empirically that container_backend.go's
	// always-on OpenStdin/AttachStdin otherwise hangs a script that reads
	// stdin forever, since a daemon-originated exec job has no attach
	// client to ever call CloseInput.
	wantArgv2 := "exec 0</dev/null; python3 tick.py"
	if len(got.Argv) != 3 || got.Argv[0] != "sh" || got.Argv[1] != "-c" || got.Argv[2] != wantArgv2 {
		t.Errorf("Argv = %v, want [sh -c %q] (J-2: run は sh -c にそのまま渡すコマンド文字列, N-4: stdin を閉じる prefix 付き)", got.Argv, wantArgv2)
	}
	if !got.Readonly {
		t.Error("Readonly = false, want true (PR-4 節: Readonly true 固定)")
	}

	// A second sweep at the EXACT SAME instant must not fire again, AND must
	// not be recorded as a single-flight Skipped either (Opus review
	// Blocker 2 (i)): `every` (10m) has not elapsed at all yet, so "not due"
	// is already sufficient reason not to fire — recording it as a
	// single-flight skip here would be the exact false-positive class
	// Blocker 2 reported (a trigger whose OWN interval hasn't come around
	// yet is not "stuck").
	result2, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("second SweepTriggers: %v", err)
	}
	if len(result2.Fired) != 0 {
		t.Errorf("second sweep (same instant) Fired = %+v, want empty (not due yet)", result2.Fired)
	}
	if len(result2.Skipped) != 0 {
		t.Errorf("second sweep (same instant) Skipped = %+v, want empty (not due yet — not a single-flight skip)", result2.Skipped)
	}

	// A THIRD sweep, once `every` has actually elapsed but the same job is
	// STILL running, is where single-flight genuinely blocks it (この PR の
	// 不変条件: 同じ (project, trigger) が同時に 2 つ走らない).
	afterEvery := now.Add(11 * time.Minute)
	result3, err := svc.SweepTriggers(context.Background(), afterEvery)
	if err != nil {
		t.Fatalf("third SweepTriggers: %v", err)
	}
	if len(result3.Fired) != 0 {
		t.Errorf("third sweep (due, still busy) Fired = %+v, want empty (single-flight)", result3.Fired)
	}
	if len(result3.Skipped) != 1 || result3.Skipped[0].TriggerKey != (TriggerKey{ProjectID: "proj-1", TriggerName: "intake"}) {
		t.Errorf("third sweep (due, still busy) Skipped = %+v, want exactly [proj-1/intake]", result3.Skipped)
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

	// Opus review Blocker 1: fireTrigger now claims single-flight by
	// inserting the trigger_runs row BEFORE calling StartExec, so a failed
	// dispatch must explicitly DELETE that claimed row (not just happen to
	// leave the DB in a state that permits a retry) — verify directly, not
	// just via the retry-fires-again behavior below.
	runsAfterFailure, err := svc.Triggers.ListInFlightTriggerRuns()
	if err != nil {
		t.Fatalf("ListInFlightTriggerRuns after dispatch failure: %v", err)
	}
	if len(runsAfterFailure) != 0 {
		t.Fatalf("in-flight trigger_runs rows after a failed dispatch = %+v, want empty (DeleteTriggerRun must have run)", runsAfterFailure)
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

// TestSweepTriggers_JobRowMissing_SelfHealsAfterGracePeriod pins N-1 (Opus
// review): a trigger_runs row whose job_id can never be resolved (here:
// simulating the 30-day taskless-job GC having removed the jobs row
// entirely, since trigger_runs.job_id is not a real FK) must NOT wedge
// single-flight for that (project, trigger) forever. Before
// TriggerRunSelfHealGrace elapses it stays in-flight and blocks (matching
// the reviewer's own reproduction: "tick 1..3: Fired=0 Skipped=1
// Completed=0"); once the grace period passes, reconcileInFlight
// force-closes it and the trigger can fire again.
func TestSweepTriggers_JobRowMissing_SelfHealsAfterGracePeriod(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "1m", Run: "true"}}},
	})
	t0 := time.Now()

	// Simulate a trigger_runs row left behind by a job whose jobs row has
	// since vanished (GC) — created directly, bypassing fireTrigger/StartExec
	// entirely, exactly like a row that survived a GC sweep would look.
	ghost := &orchestrator.TriggerRun{ProjectID: "proj-1", TriggerName: "intake", JobID: "ghost-job-gone", StartedAt: t0}
	if err := svc.Triggers.CreateTriggerRun(ghost); err != nil {
		t.Fatalf("seed ghost trigger_runs row: %v", err)
	}

	// Ticks within the grace period: blocked (single-flight sees it as
	// in-flight), never fires, never completes — the exact "permanent
	// wedge" shape N-1 reported, up until the grace period.
	for _, elapsed := range []time.Duration{1 * time.Minute, 2 * time.Minute, TriggerRunSelfHealGrace - time.Second} {
		now := t0.Add(elapsed)
		result, err := svc.SweepTriggers(context.Background(), now)
		if err != nil {
			t.Fatalf("sweep at +%v: %v", elapsed, err)
		}
		if len(result.Fired) != 0 {
			t.Fatalf("sweep at +%v: Fired = %+v, want empty (still within grace period)", elapsed, result.Fired)
		}
		if len(result.Completed) != 0 {
			t.Fatalf("sweep at +%v: Completed = %+v, want empty (still within grace period)", elapsed, result.Completed)
		}
		if len(result.Skipped) != 1 {
			t.Fatalf("sweep at +%v: Skipped = %+v, want exactly 1 (blocked by the unresolvable ghost row)", elapsed, result.Skipped)
		}
	}
	if exec.callCount() != 0 {
		t.Fatalf("StartExec calls while wedged = %d, want 0", exec.callCount())
	}

	// Past the grace period: self-heal force-closes the ghost row (recorded
	// as Completed with TriggerRunSelfHealExitCode) AND, since `every` (1m)
	// has long since elapsed and the slot is now free, fires a fresh run in
	// the SAME tick.
	healTick := t0.Add(TriggerRunSelfHealGrace + time.Second)
	result, err := svc.SweepTriggers(context.Background(), healTick)
	if err != nil {
		t.Fatalf("self-heal sweep: %v", err)
	}
	if len(result.Completed) != 1 || result.Completed[0].ExitCode != TriggerRunSelfHealExitCode {
		t.Fatalf("self-heal sweep Completed = %+v, want exactly 1 with ExitCode=%d", result.Completed, TriggerRunSelfHealExitCode)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("self-heal sweep Fired = %+v, want exactly 1 (slot freed, every long elapsed)", result.Fired)
	}
	if exec.callCount() != 1 {
		t.Fatalf("StartExec calls after self-heal = %d, want exactly 1", exec.callCount())
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

	// Second sweep once `every` (10m) has elapsed: all three are still
	// "running" (no job completed) — every one of them must be
	// independently skipped (not just one, proving the per-key scoping, not
	// a global lock). Advancing past `every` (rather than reusing the same
	// instant) matters after Opus review Blocker 2 (i): a not-yet-due
	// trigger is never counted as skipped, only a due-and-busy one is.
	afterEvery := now.Add(11 * time.Minute)
	result2, err := svc.SweepTriggers(context.Background(), afterEvery)
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

// TestSweepTriggers_NilJobs_NoOp pins N-10 (Opus review): Jobs joins the
// same all-or-nothing nil guard as Triggers/Projects/Meta, NOT because its
// absence would panic (jobTerminalState already handles a nil s.Jobs by
// returning a plain error) but because letting the sweep still FIRE
// triggers with no way to ever reconcile them back out of in-flight would
// silently wedge single-flight for every trigger after its first fire — see
// SweepTriggers' own doc comment for the full asymmetry-with-Exec argument.
func TestSweepTriggers_NilJobs_NoOp(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "true"}}},
	})
	svc.Jobs = nil

	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers with nil Jobs: %v", err)
	}
	if len(result.Fired) != 0 || len(result.Skipped) != 0 || len(result.Completed) != 0 {
		t.Errorf("result = %+v, want an entirely empty no-op", result)
	}
	if exec.callCount() != 0 {
		t.Errorf("StartExec calls = %d, want 0 (must not fire a trigger it can never reconcile)", exec.callCount())
	}
}

// ---- Concurrency (Opus review Blocker 1: single-flight's check-then-act race) ----
//
// internal/db/db.go's SetMaxOpenConns(1) serializes individual SQL
// STATEMENTS through one connection, but does nothing to serialize the
// Go-level SEQUENCE "list in-flight -> dispatch (slow) -> record" across
// two concurrent callers — each of their own statements can still interleave
// at the gaps between them. These three tests reproduce the reviewer's
// exact three patterns and assert the DB-level partial UNIQUE index
// (idx_trigger_runs_inflight_unique, migration 0043) closes all three:
// exactly one dispatch, exactly one in-flight row, no matter which caller
// "wins".
//
// fakeTriggerExecDispatcher.delay is set wide (well above what an in-memory
// sqlite read takes) so both callers' own single-flight reads are
// guaranteed to have already run before either's dispatch returns — the
// exact interleaving that broke the pre-fix code (see this PR's report,
// "Blocker 1 の並行テスト" for actual before/after output).

// runConcurrently starts every fn on its own goroutine, holds them all at a
// shared barrier until every one of them has reached it, releases them all
// at once (maximizing the chance their real work overlaps), and blocks
// until every fn has RETURNED before returning itself — so callers can
// safely inspect shared state (exec.calls, the DB) immediately after.
func runConcurrently(fns ...func()) {
	var startWG sync.WaitGroup
	startWG.Add(len(fns))
	barrier := make(chan struct{})
	var doneWG sync.WaitGroup
	doneWG.Add(len(fns))
	for _, fn := range fns {
		fn := fn
		go func() {
			defer doneWG.Done()
			startWG.Done()
			<-barrier
			fn()
		}()
	}
	startWG.Wait()
	close(barrier)
	doneWG.Wait()
}

// TestSweepTriggers_ConcurrentSweepAndRunNow_OnlyOneFires reproduces "boid
// trigger run を打った瞬間に sweep tick が来る" — a manual RunTriggerNow
// racing the periodic SweepTriggers for the exact same (project, trigger).
func TestSweepTriggers_ConcurrentSweepAndRunNow_OnlyOneFires(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "true"}}},
	})
	exec.delay = 50 * time.Millisecond
	now := time.Now()

	var sweepResult TriggerSweepResult
	var sweepErr error
	var runNowResult *TriggerRunNowResult
	var runNowErr error
	runConcurrently(
		func() { sweepResult, sweepErr = svc.SweepTriggers(context.Background(), now) },
		func() { runNowResult, runNowErr = svc.RunTriggerNow(context.Background(), "proj-1", "intake") },
	)

	if sweepErr != nil {
		t.Fatalf("SweepTriggers: %v", sweepErr)
	}
	if runNowErr != nil {
		t.Fatalf("RunTriggerNow: %v", runNowErr)
	}

	if got := exec.callCount(); got != 1 {
		t.Fatalf("StartExec calls = %d, want exactly 1 (Opus review Blocker 1: sweep‖runNow race)", got)
	}
	runs, err := svc.Triggers.ListInFlightTriggerRuns()
	if err != nil {
		t.Fatalf("ListInFlightTriggerRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("in-flight trigger_runs rows = %d, want exactly 1", len(runs))
	}

	// Exactly one of the two callers must report having fired.
	sweepFired := len(sweepResult.Fired) == 1
	runNowFired := runNowResult != nil && !runNowResult.Skipped
	if sweepFired == runNowFired {
		t.Fatalf("sweepFired=%v runNowFired=%v, want exactly one true", sweepFired, runNowFired)
	}
}

// TestRunTriggerNow_ConcurrentDoubleCall_OnlyOneFires reproduces "boid
// trigger run を 2 つ同時に叩く" — two manual runs racing each other.
func TestRunTriggerNow_ConcurrentDoubleCall_OnlyOneFires(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "true"}}},
	})
	exec.delay = 50 * time.Millisecond

	var r1, r2 *TriggerRunNowResult
	var e1, e2 error
	runConcurrently(
		func() { r1, e1 = svc.RunTriggerNow(context.Background(), "proj-1", "intake") },
		func() { r2, e2 = svc.RunTriggerNow(context.Background(), "proj-1", "intake") },
	)

	if e1 != nil {
		t.Fatalf("first RunTriggerNow: %v", e1)
	}
	if e2 != nil {
		t.Fatalf("second RunTriggerNow: %v", e2)
	}

	if got := exec.callCount(); got != 1 {
		t.Fatalf("StartExec calls = %d, want exactly 1 (Opus review Blocker 1: runNow‖runNow race)", got)
	}
	runs, err := svc.Triggers.ListInFlightTriggerRuns()
	if err != nil {
		t.Fatalf("ListInFlightTriggerRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("in-flight trigger_runs rows = %d, want exactly 1", len(runs))
	}

	fired1 := !r1.Skipped
	fired2 := !r2.Skipped
	if fired1 == fired2 {
		t.Fatalf("fired1=%v fired2=%v, want exactly one true", fired1, fired2)
	}
}

// TestSweepTriggers_TwoDaemonProcesses_OnlyOneDispatches reproduces "daemon
// が 2 プロセス": two INDEPENDENT TaskWorkflowService instances, each with
// its own *sql.DB connection pool (own SetMaxOpenConns(1) queue) and its own
// ExecDispatcher/JobStore — the ONLY thing shared between them is the sqlite
// FILE, exactly as two real daemon processes sharing one DB file would be.
// Nothing at the Go level (no shared map, no shared mutex) is available to
// serialize them — only the DB's own partial UNIQUE index can.
func TestSweepTriggers_TwoDaemonProcesses_OnlyOneDispatches(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "shared.db")
	meta := map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "true"}}},
	}
	svcA, _, execA := newTriggerSweepTestServiceOnFile(t, dbPath, true, meta)
	svcB, _, execB := newTriggerSweepTestServiceOnFile(t, dbPath, false, meta)
	execA.delay = 50 * time.Millisecond
	execB.delay = 50 * time.Millisecond
	now := time.Now()

	var resultA, resultB TriggerSweepResult
	var errA, errB error
	runConcurrently(
		func() { resultA, errA = svcA.SweepTriggers(context.Background(), now) },
		func() { resultB, errB = svcB.SweepTriggers(context.Background(), now) },
	)

	if errA != nil {
		t.Fatalf("daemon A SweepTriggers: %v", errA)
	}
	if errB != nil {
		t.Fatalf("daemon B SweepTriggers: %v", errB)
	}

	totalDispatches := execA.callCount() + execB.callCount()
	if totalDispatches != 1 {
		t.Fatalf("total StartExec calls across both daemons = %d, want exactly 1 (Opus review Blocker 1: 2-daemon race)", totalDispatches)
	}
	// Either daemon's own view of the shared file must agree: exactly one
	// in-flight row.
	runs, err := svcA.Triggers.ListInFlightTriggerRuns()
	if err != nil {
		t.Fatalf("ListInFlightTriggerRuns (via daemon A): %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("in-flight trigger_runs rows (shared file) = %d, want exactly 1", len(runs))
	}

	firedA := len(resultA.Fired) == 1
	firedB := len(resultB.Fired) == 1
	if firedA == firedB {
		t.Fatalf("firedA=%v firedB=%v, want exactly one true", firedA, firedB)
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

// fakeTriggerNotifier records every Notify call, and (for N-3, Opus review)
// whether the ctx it received carried a deadline.
type fakeTriggerNotifier struct {
	messages       []string
	sawDeadline    bool
	deadlineWithin time.Duration // ctx's deadline minus time.Now(), captured on the first call
}

func (f *fakeTriggerNotifier) Notify(ctx context.Context, ev notify.Event) error {
	f.messages = append(f.messages, ev.Message)
	if dl, ok := ctx.Deadline(); ok {
		f.sawDeadline = true
		f.deadlineWithin = time.Until(dl)
	}
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

// TestTriggerLoop_SkipStreak_NotifiesWhenOverrunExceedsMultipleOfEvery pins
// Opus review Blocker 2 (ii): the notification threshold is
// TriggerStuckOverrunMultiplier × the trigger's OWN `every`, not a raw sweep
// tick count — so it scales correctly regardless of how the sweep interval
// relates to `every` (see TriggerStuckOverrunMultiplier's own doc comment).
func TestTriggerLoop_SkipStreak_NotifiesWhenOverrunExceedsMultipleOfEvery(t *testing.T) {
	key := TriggerKey{ProjectID: "proj-1", TriggerName: "intake"}
	notifier := &fakeTriggerNotifier{}
	loop := &TriggerLoop{Notifier: notifier}

	every := 10 * time.Minute
	threshold := stuckThreshold(every) // 10m × 1.5 = 15m
	since := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)

	// Just under the threshold: no notification yet.
	loop.trackSkipStreak(context.Background(), since.Add(threshold-time.Minute), []TriggerSkip{{TriggerKey: key, Since: since, Every: every}})
	if len(notifier.messages) != 0 {
		t.Fatalf("messages just under the threshold = %v, want none yet", notifier.messages)
	}

	// At/past the threshold (1st multiple crossed): notifies once.
	loop.trackSkipStreak(context.Background(), since.Add(threshold), []TriggerSkip{{TriggerKey: key, Since: since, Every: every}})
	if len(notifier.messages) != 1 {
		t.Fatalf("messages at the threshold = %v, want exactly 1", notifier.messages)
	}

	// Still within the SAME multiple: no repeat notification.
	loop.trackSkipStreak(context.Background(), since.Add(threshold+time.Minute), []TriggerSkip{{TriggerKey: key, Since: since, Every: every}})
	if len(notifier.messages) != 1 {
		t.Fatalf("messages still within the same multiple = %v, want still exactly 1", notifier.messages)
	}

	// Crossing a SECOND multiple of the threshold: notifies again — a very
	// long stuck episode still gets periodic reminders.
	loop.trackSkipStreak(context.Background(), since.Add(2*threshold), []TriggerSkip{{TriggerKey: key, Since: since, Every: every}})
	if len(notifier.messages) != 2 {
		t.Fatalf("messages after a second multiple crossed = %v, want exactly 2", notifier.messages)
	}

	// A tick where the key is NOT skipped (it fired, or resolved) resets the
	// streak — a fresh episode starting well under threshold must not
	// notify again immediately.
	loop.trackSkipStreak(context.Background(), since.Add(3*threshold), nil)
	newSince := since.Add(3 * threshold)
	loop.trackSkipStreak(context.Background(), newSince.Add(time.Minute), []TriggerSkip{{TriggerKey: key, Since: newSince, Every: every}})
	if len(notifier.messages) != 2 {
		t.Fatalf("messages after reset + a fresh sub-threshold skip = %v, want still exactly 2 (streak was reset)", notifier.messages)
	}
}

// TestTriggerLoop_SkipStreak_ThresholdScalesWithEvery pins that the
// threshold stays PROPORTIONAL to each trigger's own `every` after the
// multiplier moved 3 → 1.5 (nose 2026-08-24). The fractional multiplier is
// the part worth pinning: an int conversion of 1.5 truncates to 1ns, so a
// naive `every * time.Duration(TriggerStuckOverrunMultiplier)` would fire on
// literally every tick.
func TestTriggerLoop_SkipStreak_ThresholdScalesWithEvery(t *testing.T) {
	for _, tc := range []struct{ every, want time.Duration }{
		{10 * time.Minute, 15 * time.Minute},
		{time.Hour, 90 * time.Minute},
		{30 * time.Second, 45 * time.Second},
	} {
		if got := stuckThreshold(tc.every); got != tc.want {
			t.Errorf("stuckThreshold(%s) = %s, want %s", tc.every, got, tc.want)
		}
	}
}

// TestTriggerLoop_SkipStreak_FifteenMinuteStallNotifies pins the case this
// change exists for: khi's sweep (`every: 10m`) stalled twice on 2026-08-24
// for 25 and 26 minutes each, and neither episode notified — the old 3×
// threshold was 30m and a human noticed first both times. At 1.5× the same
// stalls cross the line at 15m.
func TestTriggerLoop_SkipStreak_FifteenMinuteStallNotifies(t *testing.T) {
	key := TriggerKey{ProjectID: "proj-1", TriggerName: "sweep"}
	notifier := &fakeTriggerNotifier{}
	loop := &TriggerLoop{Notifier: notifier}

	every := 10 * time.Minute
	since := time.Date(2026, 8, 24, 1, 37, 34, 0, time.UTC) // the real stall's StartedAt

	loop.trackSkipStreak(context.Background(), since.Add(14*time.Minute), []TriggerSkip{{TriggerKey: key, Since: since, Every: every}})
	if len(notifier.messages) != 0 {
		t.Fatalf("notified at 14m: %v, want none yet", notifier.messages)
	}
	loop.trackSkipStreak(context.Background(), since.Add(15*time.Minute), []TriggerSkip{{TriggerKey: key, Since: since, Every: every}})
	if len(notifier.messages) != 1 {
		t.Fatalf("messages at 15m = %v, want exactly 1", notifier.messages)
	}
}

// TestTriggerLoop_SkipStreak_NoFalsePositive_ExecutionShorterThanEvery pins
// the reviewer's own reproduction scenario (Opus review Blocker 2): a
// trigger with Interval=1m / every=10m whose command normally takes 5
// minutes must never notify — not even once — across many sweep ticks,
// because it is never simultaneously due AND busy.
func TestTriggerLoop_SkipStreak_NoFalsePositive_ExecutionShorterThanEvery(t *testing.T) {
	svc, jobs, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {Triggers: []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "true"}}},
	})
	notifier := &fakeTriggerNotifier{}
	loop := &TriggerLoop{Store: svc, Notifier: notifier}
	start := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)

	// t=0m: fires (never run before). Simulates the command completing 5
	// minutes later, like the reviewer's cycle_preflight.sh-shaped example.
	result, err := svc.SweepTriggers(context.Background(), start)
	if err != nil {
		t.Fatalf("t=0m sweep: %v", err)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("t=0m Fired = %+v, want exactly 1", result.Fired)
	}
	loop.trackSkipStreak(context.Background(), start, result.Skipped)

	// t=1m..4m: every 1-minute sweep tick, the job is still running and NOT
	// yet due (every=10m) — must never be counted as Skipped.
	for m := 1; m <= 4; m++ {
		now := start.Add(time.Duration(m) * time.Minute)
		r, err := svc.SweepTriggers(context.Background(), now)
		if err != nil {
			t.Fatalf("t=%dm sweep: %v", m, err)
		}
		if len(r.Skipped) != 0 {
			t.Fatalf("t=%dm Skipped = %+v, want empty (not due yet)", m, r.Skipped)
		}
		loop.trackSkipStreak(context.Background(), now, r.Skipped)
	}

	// t=5m: the command finishes.
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls before completion = %d, want 1", len(exec.calls))
	}
	jobs.complete("job-1", 0)

	// t=5m..9m: still not due (every=10m) — the completed run reconciles
	// out of in-flight cleanly, no skip.
	for m := 5; m <= 9; m++ {
		now := start.Add(time.Duration(m) * time.Minute)
		r, err := svc.SweepTriggers(context.Background(), now)
		if err != nil {
			t.Fatalf("t=%dm sweep: %v", m, err)
		}
		if len(r.Skipped) != 0 {
			t.Fatalf("t=%dm Skipped = %+v, want empty", m, r.Skipped)
		}
		loop.trackSkipStreak(context.Background(), now, r.Skipped)
	}

	// t=10m: due again, not busy (finished at t=5m) — fires normally, not a
	// skip.
	final, err := svc.SweepTriggers(context.Background(), start.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("t=10m sweep: %v", err)
	}
	if len(final.Fired) != 1 {
		t.Fatalf("t=10m Fired = %+v, want exactly 1 (every elapsed, not busy)", final.Fired)
	}
	if len(final.Skipped) != 0 {
		t.Fatalf("t=10m Skipped = %+v, want empty", final.Skipped)
	}
	loop.trackSkipStreak(context.Background(), start.Add(10*time.Minute), final.Skipped)

	if len(notifier.messages) != 0 {
		t.Fatalf("notifications across the whole cycle = %v, want ZERO (Opus review Blocker 2 reproduction)", notifier.messages)
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

// TestTriggerLoop_Notify_WrapsCtxWithNotifyTimeout pins N-3 (Opus review):
// notify.Service.Notify runs the configured notify command via
// exec.CommandContext SYNCHRONOUSLY, so notify() must never hand it the
// TriggerLoop's own long-lived server ctx unbounded — a hanging notify
// command would otherwise wedge runOnce (and therefore every future sweep
// tick) until daemon shutdown. queue_notify.go's notifySuggestionArrived and
// triage_done.go's own notify call are both already wrapped in
// context.WithTimeout(ctx, notifyTimeout); this pins trigger_loop.go's
// notify matching that.
func TestTriggerLoop_Notify_WrapsCtxWithNotifyTimeout(t *testing.T) {
	key := TriggerKey{ProjectID: "proj-1", TriggerName: "intake"}
	notifier := &fakeTriggerNotifier{}
	loop := &TriggerLoop{Notifier: notifier}

	// context.Background() itself has no deadline — if notify() passes ctx
	// straight through unwrapped, fakeTriggerNotifier would see none either.
	loop.notify(context.Background(), key, "test message")

	if len(notifier.messages) != 1 {
		t.Fatalf("messages = %v, want exactly 1", notifier.messages)
	}
	if !notifier.sawDeadline {
		t.Fatal("Notify's ctx had no deadline — notify() must wrap it with context.WithTimeout(ctx, notifyTimeout), matching queue_notify.go/triage_done.go")
	}
	// Sanity: the deadline is roughly notifyTimeout out, not e.g. a stray
	// very-short or very-long value.
	if notifier.deadlineWithin <= 0 || notifier.deadlineWithin > notifyTimeout {
		t.Errorf("deadline was %v from now, want within (0, %v]", notifier.deadlineWithin, notifyTimeout)
	}
}

// ---- on:signals trigger predicate (docs/plans/signal-ingest-detailed-design.md
// §4.2, PR-6, Q15 採点表: "trigger の debounce と single-flight にテストがある") ----
//
// debounce and single-flight are NOT new mechanisms here (design doc §4.2):
// debounce falls out structurally from ANDing the existing every-elapsed
// check with HasPendingSignals (an every window collapses any number of
// Signals into at most one fire, and a fire that leaves Signals unacked
// re-arms once every elapses again — crash recovery/coalescing from the
// same one line); single-flight is untouched — trigger_runs'
// idx_trigger_runs_inflight_unique (migration 0043) still owns it, as the
// pre-existing single-flight tests above (unchanged, still green) confirm.

// fakeSignalStore stubs api.SignalStore for these tests — SweepTriggers'
// on:signals predicate only ever calls HasPendingSignals; the other methods
// exist purely so *fakeSignalStore satisfies the SignalStore interface and
// error loudly if some future change starts calling them from this path.
type fakeSignalStore struct {
	mu      sync.Mutex
	pending map[string]bool // workspaceID -> "has a pending signal"
	err     error           // when non-nil, HasPendingSignals returns this instead
	calls   []string        // workspaceIDs HasPendingSignals was invoked with
}

func (f *fakeSignalStore) HasPendingSignals(workspaceID string, _ int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, workspaceID)
	if f.err != nil {
		return false, f.err
	}
	return f.pending[workspaceID], nil
}

func (f *fakeSignalStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSignalStore) IngestSignals(string, string, string, []orchestrator.SignalIngestRow) error {
	return fmt.Errorf("fakeSignalStore: IngestSignals not implemented (unused by SweepTriggers)")
}
func (f *fakeSignalStore) GetSignalCursor(string, string, string) (string, error) {
	return "", fmt.Errorf("fakeSignalStore: GetSignalCursor not implemented (unused by SweepTriggers)")
}
func (f *fakeSignalStore) ListSignals(orchestrator.SignalFilter) ([]*orchestrator.Signal, error) {
	return nil, fmt.Errorf("fakeSignalStore: ListSignals not implemented (unused by SweepTriggers)")
}
func (f *fakeSignalStore) ClaimSignals(string, int, int) ([]*orchestrator.Signal, error) {
	return nil, fmt.Errorf("fakeSignalStore: ClaimSignals not implemented (unused by SweepTriggers)")
}
func (f *fakeSignalStore) AckSignals(string, []string) error {
	return fmt.Errorf("fakeSignalStore: AckSignals not implemented (unused by SweepTriggers)")
}

var _ SignalStore = (*fakeSignalStore)(nil)

// TestSweepTriggers_OnSignals_NoPendingSignals_NotDue pins §4.2's `due :=
// ... && HasPendingSignals(...)`: an on:signals trigger whose `every` HAS
// elapsed (here: never run before, the unconditionally-due case) still does
// not fire while its workspace has no pending Signal.
func TestSweepTriggers_OnSignals_NoPendingSignals_NotDue(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "sweep", Every: "2m", Run: "python3 -m khi.app.scan", On: orchestrator.TriggerOnSignals}},
		},
	})
	signals := &fakeSignalStore{pending: map[string]bool{}}
	svc.Signals = signals

	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers: %v", err)
	}
	if len(result.Fired) != 0 {
		t.Errorf("Fired = %+v, want empty (no pending signal)", result.Fired)
	}
	if len(exec.calls) != 0 {
		t.Errorf("StartExec calls = %d, want 0", len(exec.calls))
	}
	if signals.callCount() == 0 {
		t.Error("HasPendingSignals was never called — on:signals predicate not wired")
	}
}

// TestSweepTriggers_OnSignals_PendingSignals_EveryElapsed_Fires is the
// positive counterpart: every elapsed AND a pending signal exists → fires.
func TestSweepTriggers_OnSignals_PendingSignals_EveryElapsed_Fires(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "sweep", Every: "2m", Run: "python3 -m khi.app.scan", On: orchestrator.TriggerOnSignals}},
		},
	})
	svc.Signals = &fakeSignalStore{pending: map[string]bool{"ws-1": true}}

	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers: %v", err)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("Fired = %+v, want exactly 1", result.Fired)
	}
	if result.Fired[0].TriggerKey != (TriggerKey{ProjectID: "proj-1", TriggerName: "sweep"}) {
		t.Errorf("Fired[0] = %+v, unexpected", result.Fired[0])
	}
	if len(exec.calls) != 1 {
		t.Errorf("StartExec calls = %d, want 1", len(exec.calls))
	}
}

// TestSweepTriggers_OnSignals_EveryNotElapsed_Debounced pins the debounce
// half of §4.2: once a trigger has fired, a SECOND pending signal arriving
// (or simply still being present) before `every` elapses again must NOT
// cause a second fire — any number of Signals inside one `every` window
// collapse into at most one fire.
func TestSweepTriggers_OnSignals_EveryNotElapsed_Debounced(t *testing.T) {
	svc, jobs, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "sweep", Every: "10m", Run: "python3 -m khi.app.scan", On: orchestrator.TriggerOnSignals}},
		},
	})
	signals := &fakeSignalStore{pending: map[string]bool{"ws-1": true}}
	svc.Signals = signals
	now := time.Now()

	result, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("first sweep Fired = %+v, want exactly 1", result.Fired)
	}
	// The trigger's own run must complete (else this would be a
	// single-flight skip, not a debounce non-fire) so this test isolates
	// the every-elapsed half of the AND, same as
	// TestSweepTriggers_EveryElapsed_FiresAgainOnlyAfterInterval does for
	// plain schedule triggers.
	jobs.complete("job-1", 0)

	// The signal is STILL pending (script has not acked it yet) but only 5m
	// of the 10m window have passed.
	before := now.Add(5 * time.Minute)
	result2, err := svc.SweepTriggers(context.Background(), before)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(result2.Fired) != 0 {
		t.Errorf("second sweep (every not elapsed) Fired = %+v, want empty (debounce)", result2.Fired)
	}
	if len(exec.calls) != 1 {
		t.Errorf("StartExec calls after second sweep = %d, want still 1", len(exec.calls))
	}
}

// TestSweepTriggers_OnSignals_StillPendingAfterEvery_RefiresForCrashRecovery
// pins the other half of §4.2's single sentence: "発火後も未 ack が残っていれば
// (= 判断が crash した/捌き切れなかった) every 経過後に再発火する" — crash
// recovery and coalescing are the SAME mechanism (an unresolved pending
// signal simply keeps the predicate true), so once `every` elapses again the
// trigger re-fires without any separate retry/backoff bookkeeping.
func TestSweepTriggers_OnSignals_StillPendingAfterEvery_RefiresForCrashRecovery(t *testing.T) {
	svc, jobs, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "sweep", Every: "10m", Run: "python3 -m khi.app.scan", On: orchestrator.TriggerOnSignals}},
		},
	})
	svc.Signals = &fakeSignalStore{pending: map[string]bool{"ws-1": true}}
	now := time.Now()

	if _, err := svc.SweepTriggers(context.Background(), now); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls after first sweep = %d, want 1", len(exec.calls))
	}
	// The judgment task "crashed" (or simply never got around to acking) —
	// its job still reaches a terminal state (non-zero exit models the
	// crash), but the signal it was supposed to ack is still pending.
	jobs.complete("job-1", 1)

	after := now.Add(11 * time.Minute)
	result, err := svc.SweepTriggers(context.Background(), after)
	if err != nil {
		t.Fatalf("second sweep (after every): %v", err)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("second sweep Fired = %+v, want exactly 1 (re-fire: crash recovery)", result.Fired)
	}
	if len(exec.calls) != 2 {
		t.Errorf("StartExec calls after second sweep = %d, want 2", len(exec.calls))
	}
}

// TestSweepTriggers_OnSignals_NoWorkspace_NeverDue pins §4.2's "workspace
// 未所属 project の on: signals trigger は常に not due (debug log のみ、
// エラーにしない)": a project with no linked workspace (empty
// SecretNamespace — ProjectStore.GetWithWorkspace's hydration convention,
// see that method's own doc comment) must never fire an on:signals trigger,
// and SweepTriggers as a whole must not error over it, regardless of what
// HasPendingSignals would have said — it must not even be called with an
// empty workspace id (orchestrator.HasPendingSignals treats "" as a caller
// error).
func TestSweepTriggers_OnSignals_NoWorkspace_NeverDue(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "", // not linked to any workspace
			Triggers:        []orchestrator.Trigger{{Name: "sweep", Every: "2m", Run: "python3 -m khi.app.scan", On: orchestrator.TriggerOnSignals}},
		},
	})
	signals := &fakeSignalStore{err: fmt.Errorf("must not be called with an empty workspace id")}
	svc.Signals = signals

	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers: %v, want nil (workspace-unlinked on:signals trigger must not error the sweep)", err)
	}
	if len(result.Fired) != 0 {
		t.Errorf("Fired = %+v, want empty (no linked workspace)", result.Fired)
	}
	if len(exec.calls) != 0 {
		t.Errorf("StartExec calls = %d, want 0", len(exec.calls))
	}
	if signals.callCount() != 0 {
		t.Errorf("HasPendingSignals was called %d time(s) with an empty-workspace project, want 0", signals.callCount())
	}
}

// TestSweepTriggers_OnSignals_NoSignalStoreWired_NeverDue covers a daemon
// built without a SignalStore wired (svc.Signals nil, same "optional
// dependency" convention Triggers/Exec/Notifier already follow on this
// struct) — an on:signals trigger must fail closed (never due), not panic.
func TestSweepTriggers_OnSignals_NoSignalStoreWired_NeverDue(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "sweep", Every: "2m", Run: "python3 -m khi.app.scan", On: orchestrator.TriggerOnSignals}},
		},
	})
	// svc.Signals deliberately left nil.

	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers: %v, want nil", err)
	}
	if len(result.Fired) != 0 {
		t.Errorf("Fired = %+v, want empty (no SignalStore wired)", result.Fired)
	}
	if len(exec.calls) != 0 {
		t.Errorf("StartExec calls = %d, want 0", len(exec.calls))
	}
}

// TestSweepTriggers_OnSignals_HasPendingSignalsErrors_FailsClosed pins F2
// (Opus review 2026-08-26, CONFIRMED mutation survivor):
// signalsPendingForTrigger's `return false` on a HasPendingSignals error
// (fail-closed, "never due" — matching the workspace-unlinked/no-store
// cases right above) was not actually exercised by any prior test — flipping
// it to `return true` would not have turned any existing test red. A
// HasPendingSignals error must make the trigger not due AND must not
// surface as a SweepTriggers-level error (the same fail-open-per-trigger
// posture triggerIsDue/dispatch failures already get — logged, not
// propagated).
func TestSweepTriggers_OnSignals_HasPendingSignalsErrors_FailsClosed(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "sweep", Every: "2m", Run: "python3 -m khi.app.scan", On: orchestrator.TriggerOnSignals}},
		},
	})
	svc.Signals = &fakeSignalStore{err: fmt.Errorf("simulated HasPendingSignals failure")}

	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers: %v, want nil (a per-trigger HasPendingSignals error must not abort the sweep)", err)
	}
	if len(result.Fired) != 0 {
		t.Errorf("Fired = %+v, want empty (HasPendingSignals error must fail closed, not open)", result.Fired)
	}
	if len(exec.calls) != 0 {
		t.Errorf("StartExec calls = %d, want 0", len(exec.calls))
	}
}

// TestSweepTriggers_OnSignals_WedgedInFlight_StaysInSkipped pins F1 (Opus
// review 2026-08-26, CONFIRMED): an on:signals trigger whose job is wedged
// (Running forever — e.g. the scan script or judgment agent hung) must keep
// appearing in Skipped on every subsequent every-elapsed tick, exactly like
// an identically-configured `on: schedule` trigger does — so
// TriggerLoop.trackSkipStreak's stuck-notification safety net still fires
// for it.
//
// Before the fix, a busy on:signals trigger whose inbox went empty (e.g.
// because the very job that's now wedged acked everything right before
// hanging) would compute due=false via the signals predicate BEFORE the
// busy check ever ran, so it never landed in Skipped at all — silently
// wedged forever with no stuck notification, unlike `on: schedule`. This
// test reproduces exactly that inbox-goes-empty-while-wedged sequence and
// asserts the trigger is recorded as Skipped anyway, on more than one
// subsequent tick (not just once).
func TestSweepTriggers_OnSignals_WedgedInFlight_StaysInSkipped(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "sweep", Every: "10m", Run: "python3 -m khi.app.scan", On: orchestrator.TriggerOnSignals}},
		},
	})
	signals := &fakeSignalStore{pending: map[string]bool{"ws-1": true}}
	svc.Signals = signals
	now := time.Now()
	key := TriggerKey{ProjectID: "proj-1", TriggerName: "sweep"}

	// First sweep fires the job — its scan script starts working through
	// the inbox.
	result, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("first sweep Fired = %+v, want exactly 1", result.Fired)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls after first sweep = %d, want 1", len(exec.calls))
	}

	// The job acks every pending Signal (inbox goes empty) but then hangs
	// right before exiting — it never reaches a terminal status (Running
	// forever, no jobs.complete call).
	signals.mu.Lock()
	signals.pending["ws-1"] = false
	signals.mu.Unlock()

	// Two subsequent every-elapsed ticks: the wedged trigger must be
	// recorded as Skipped on EACH of them (trackSkipStreak's own doc
	// comment: a stuck key stays in Skipped or Fired every tick until it
	// resolves), never silently dropped, and must never fire a second job.
	for i, at := range []time.Time{now.Add(11 * time.Minute), now.Add(22 * time.Minute)} {
		result, err := svc.SweepTriggers(context.Background(), at)
		if err != nil {
			t.Fatalf("sweep #%d: %v", i+2, err)
		}
		if len(result.Fired) != 0 {
			t.Errorf("sweep #%d Fired = %+v, want empty (still busy)", i+2, result.Fired)
		}
		if len(result.Skipped) != 1 || result.Skipped[0].TriggerKey != key {
			t.Fatalf("sweep #%d Skipped = %+v, want exactly [%+v] (F1: wedged on:signals trigger must stay in Skipped even though its inbox is now empty)", i+2, result.Skipped, key)
		}
	}
	if len(exec.calls) != 1 {
		t.Errorf("StartExec calls after wedge = %d, want still 1 (single-flight)", len(exec.calls))
	}
}

// TestSweepTriggers_OnEmpty_RegressionUnaffectedBySignals pins §4.1/§4.2's
// full-compatibility requirement ("on 省略時は従来の schedule 動作と完全互換"):
// a trigger with On=="" behaves EXACTLY like before this PR — due purely
// from `every` elapsed — and must never even consult the SignalStore
// (a wired one that errors on any call proves it is never touched).
func TestSweepTriggers_OnEmpty_RegressionUnaffectedBySignals(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "python3 tick.py"}}, // On left as the zero value
		},
	})
	signals := &fakeSignalStore{err: fmt.Errorf("must not be called for a schedule (on=\"\") trigger")}
	svc.Signals = signals

	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers: %v", err)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("Fired = %+v, want exactly 1 (on=\"\" fires purely on every-elapsed, same as pre-PR-6)", result.Fired)
	}
	if len(exec.calls) != 1 {
		t.Errorf("StartExec calls = %d, want 1", len(exec.calls))
	}
	if signals.callCount() != 0 {
		t.Errorf("HasPendingSignals was called %d time(s) for an on=\"\" trigger, want 0", signals.callCount())
	}
}

// TestSweepTriggers_OnScheduleExplicit_RegressionUnaffectedBySignals is the
// same regression pin as above but for On==TriggerOnSchedule spelled out
// explicitly, rather than left as the zero value — both must behave
// identically (ValidateTriggers already treats them as aliases).
func TestSweepTriggers_OnScheduleExplicit_RegressionUnaffectedBySignals(t *testing.T) {
	svc, _, exec := newTriggerSweepTestService(t, map[string]*orchestrator.ProjectMeta{
		"proj-1": {
			SecretNamespace: "ws-1",
			Triggers:        []orchestrator.Trigger{{Name: "intake", Every: "10m", Run: "python3 tick.py", On: orchestrator.TriggerOnSchedule}},
		},
	})
	signals := &fakeSignalStore{err: fmt.Errorf("must not be called for an on:schedule trigger")}
	svc.Signals = signals

	result, err := svc.SweepTriggers(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriggers: %v", err)
	}
	if len(result.Fired) != 1 {
		t.Fatalf("Fired = %+v, want exactly 1", result.Fired)
	}
	if len(exec.calls) != 1 {
		t.Errorf("StartExec calls = %d, want 1", len(exec.calls))
	}
	if signals.callCount() != 0 {
		t.Errorf("HasPendingSignals was called %d time(s) for an on:schedule trigger, want 0", signals.callCount())
	}
}
