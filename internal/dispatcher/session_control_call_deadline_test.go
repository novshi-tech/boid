package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins next-session-container-backend-loose-ends.md §2: every
// foreground SandboxSession control call (Resize / Stop / Signal / the
// Adopt lookup that precedes them, INCLUDING Adopt's own in-flight-join
// wait) must be bounded, so a wedged docker/podman engine socket that
// accepts a request and never answers costs the caller a bounded delay and
// an error, not a permanent hang. session.Wait is deliberately NOT covered
// here — it blocks for the job's entire lifetime by design and must keep an
// unbounded context (see session.Wait's own doc comment).
//
// Extended after an Opus review of the first version of this fix found
// three real gaps (see PR #857's history): (Major 1) Adopt's in-flight-join
// wait ignored the caller's ctx entirely, so a bounded StopJobRuntime/
// SignalJobRuntime caller still hung if some OTHER caller's Adopt for the
// same runtimeID was the one stuck against the engine; (Major 2)
// runtime_subscriber_export.go's Subscribe/WriteInput/ResizeRuntime/
// CloseInput passed context.Background() into Adopt directly — the exact
// symptom (WS resize) this PR's own body claimed to have fixed, but hadn't;
// (Major 3) a doc comment on hangingControlBackend below claimed Adopt's
// ctx propagation was covered by a daemon_state_volume_test.go test that
// in fact exercises a different function (DetectDaemonStateVolumes) and
// never calls Adopt/doAdopt at all. All three are pinned by tests in this
// file now.
//
// Extended again for two follow-ups PR #857 deliberately deferred (Opus
// review of PR #857, and next-session-container-backend-followups.md #2/
// #3): (#2) Runner.CanAttach was the one foreground control call that never
// got a deadline floor at all — it forwards its caller's own ctx straight
// into Adopt, and job_runtime_routes.go's resolveAttachableJob passes
// req.Context(), which an unbounded engine leaves open for as long as the
// HTTP client stays connected. Adopt's in-flight-join wait already selects
// on ctx (Major 1 above), so this did not hang CanAttach itself — but an
// unbounded CanAttach owning the in-flight attempt for a runtimeID starved
// every OTHER bounded joiner of that runtimeID (StopJobRuntime,
// SignalJobRuntime, ...) of its own sessionControlCallTimeout budget,
// turning a transient engine hiccup into a `boid task stop` that fails
// every single time. (#3) runtime_subscriber_export.go's Subscribe/
// WriteInput/ResizeRuntime/CloseInput logged nothing when their own Adopt
// call hit the deadline — an operator debugging a "WS attach silently does
// nothing" report could not tell that apart from the routine "no such
// runtime" result. Both are pinned by tests in this file now.

// withSessionControlCallTimeout shrinks sessionControlCallTimeout for the
// duration of one test — this package's established pattern for pinning a
// timeout without a real test actually waiting out the production value
// (see e.g. withSelfInspectTimeout in daemon_state_volume_test.go).
func withSessionControlCallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sessionControlCallTimeout.Set(d)
	t.Cleanup(func() { sessionControlCallTimeout.Set(prev) })
}

// --- containerSession.Resize --------------------------------------------

// TestContainerSession_Resize_HangingEngineHitsDeadline pins the
// container_backend.go Resize fix: Resize's backend.SandboxSession
// interface method takes no context to inherit a deadline from, so it must
// synthesize its own bounded one rather than calling ContainerResize under
// a bare context.Background().
func TestContainerSession_Resize_HangingEngineHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)

	api := &fakeDockerAPI{
		ContainerResizeFunc: func(ctx context.Context, containerID string, options client.ContainerResizeOptions) (client.ContainerResizeResult, error) {
			<-ctx.Done()
			return client.ContainerResizeResult{}, ctx.Err()
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	sess := mustLaunch(t, be, sandbox.Spec{ID: "job-resize", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-resize"})

	start := time.Now()
	err := sess.Resize(backend.TerminalSize{Rows: 40, Cols: 100})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Resize took %v against an engine that never answers; a WS resize frame would hang the connection", elapsed)
	}
	if err == nil {
		t.Fatal("want an error when the engine never answers")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
}

// --- containerBackend.Adopt's in-flight join (Major 1) --------------------

// TestContainerBackend_Adopt_InFlightJoinRespectsContextDeadline pins Major
// 1 directly at the containerBackend level: when a runtimeID's Adopt is
// already in flight (some other caller reached the cache-miss path first),
// a LATER caller with its own bounded ctx must not be held hostage by the
// FIRST caller's own context — which, pre-fix, was very often
// context.Background() (every real call site into Adopt used it before
// this PR). Before Major 1's fix, `<-attempt.done` alone ignored ctx
// entirely.
func TestContainerBackend_Adopt_InFlightJoinRespectsContextDeadline(t *testing.T) {
	inspectStarted := make(chan struct{})
	var closeOnce sync.Once
	api := &fakeDockerAPI{
		ContainerInspectFunc: func(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			closeOnce.Do(func() { close(inspectStarted) })
			<-ctx.Done()
			return client.ContainerInspectResult{}, ctx.Err()
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	// The "owner" attempt: Adopt with an unbounded context, modelling every
	// pre-fix real caller. containerBackend.Adopt registers the runtimeID in
	// b.adopting BEFORE calling doAdopt/ContainerInspect (see Adopt's own
	// doc comment), so by the time ContainerInspectFunc runs (and closes
	// inspectStarted below), the second Adopt call below is guaranteed to
	// observe an in-flight attempt rather than racing to become the owner
	// itself.
	go func() {
		_, _ = be.Adopt(context.Background(), "rt-inflight")
	}()
	<-inspectStarted

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, ok := be.Adopt(ctx, "rt-inflight")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Adopt (in-flight join) took %v against an owner attempt stuck on an unbounded context", elapsed)
	}
	if ok {
		t.Fatal("want ok=false: the in-flight attempt never resolved before this caller's ctx expired")
	}
}

// --- Runner.Subscribe / WriteInput / ResizeRuntime / CloseInput (Major 2) -

// hangingAdoptBackend is a backend.SandboxBackend whose Adopt blocks until
// the context it is given is done, then reports ok=false — modelling a
// cache-miss Adopt against a wedged engine (containerBackend.doAdopt's
// ContainerInspect never returning). Used to pin Major 2:
// runtime_subscriber_export.go's Subscribe/WriteInput/ResizeRuntime/
// CloseInput must pass a bounded context into Adopt, not
// context.Background().
type hangingAdoptBackend struct{}

var _ backend.SandboxBackend = (*hangingAdoptBackend)(nil)

func (b *hangingAdoptBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return nil, fmt.Errorf("hangingAdoptBackend.Launch is not implemented")
}
func (b *hangingAdoptBackend) Adopt(ctx context.Context, _ string) (backend.SandboxSession, bool) {
	<-ctx.Done()
	return nil, false
}
func (b *hangingAdoptBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// newRunnerTestDBWithJob returns a migrated in-memory *sql.DB containing one
// project/task/job row with runtime_id set to runtimeID, plus the created
// job's ID — the minimum runtimeIDForJob (runtime_subscriber_export.go)
// needs to resolve jobID to runtimeID. Opens the DB directly (not
// testutil.NewTestDB) for the same import-cycle reason as
// gitgateway_wire_test.go's newGatewayTestDB: testutil transitively imports
// internal/server, which imports internal/dispatcher.
func newRunnerTestDBWithJob(t *testing.T, runtimeID string) (dbConn *sql.DB, jobID string) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Conn.Close() })

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	job := &Job{TaskID: task.ID, ProjectID: "proj-1", HandlerID: "h", RuntimeID: runtimeID}
	if err := CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	return d.Conn, job.ID
}

// subscribeResult carries Runner.Subscribe's two boolean returns out of the
// goroutine every timeout-shaped test in this file runs it in — see
// TestRunner_CanAttach_UnboundedCallerContextHitsDeadline's NB-3 note for
// why the calls under test are never made inline here.
type subscribeResult struct {
	ok       bool
	finished bool
}

// TestRunner_Subscribe_HangingAdoptHitsDeadline pins Major 2 for the WS
// attach / Web UI SSE follow ingress: Subscribe must not hang forever when
// Adopt's underlying engine call (a cache-miss ContainerInspect) never
// answers.
//
// Also pins NB2 (Opus review of PR #864, 2nd round): this is exactly the
// "Adopt itself timed out" scenario Subscribe's own doc comment describes
// — hangingAdoptBackend.Adopt blocks until ctx.Done() fires, so ctx.Err()
// is guaranteed non-nil by the time Subscribe's !adopted branch reads it.
// finished must be false here: the job could very well still be running
// (nothing here tells us the container has exited — the engine simply
// never answered in time), so reporting finished=true (this branch's
// pre-NB2 hardcoded value) would tell a WS/SSE caller the job is done when
// it might not be — the exact false positive class B2 already fixed for
// the "adopted successfully but lost its stream" case, reopened through
// this different door.
//
// [next-session-container-backend-followups.md #2]: goroutine + select, not
// an inline call plus an elapsed check. PR #862 converted the eight call
// sites it touched and left this one (and the five below) in the inline
// shape; both forms PASS identically today, but they FAIL differently — the
// inline form never reaches its own elapsed assertion when the deadline it
// pins regresses, it just blocks in Adopt until go test's -timeout (10
// minutes by default) kills the whole package with a goroutine dump.
func TestRunner_Subscribe_HangingAdoptHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &hangingAdoptBackend{}}

	done := make(chan subscribeResult, 1)
	go func() {
		_, _, _, ok, finished := r.Subscribe(jobID)
		done <- subscribeResult{ok: ok, finished: finished}
	}()

	select {
	case res := <-done:
		if res.ok {
			t.Fatal("want ok=false: Adopt never resolved before the deadline")
		}
		if res.finished {
			t.Error("want finished=false: Adopt timing out against a wedged engine tells us nothing about whether the job has actually exited")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return within 5s against an Adopt whose engine call never answers; a WS attach would hang")
	}
}

// promptlyNotFoundBackend is a backend.SandboxBackend whose Adopt returns
// ok=false IMMEDIATELY, without ever touching ctx — modelling Adopt
// legitimately answering "no such session" (the backend has no notion of
// this runtimeID at all: already exited and reaped, or never existed) with
// plenty of ctx budget left, the opposite of hangingAdoptBackend's
// ctx-exhausted case. Used to pin the OTHER half of NB2: Subscribe's
// !adopted branch must still report finished=true when ctx.Err() is nil.
type promptlyNotFoundBackend struct{}

var _ backend.SandboxBackend = (*promptlyNotFoundBackend)(nil)

func (b *promptlyNotFoundBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return nil, fmt.Errorf("promptlyNotFoundBackend.Launch is not implemented")
}
func (b *promptlyNotFoundBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}
func (b *promptlyNotFoundBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// TestRunner_Subscribe_AdoptPromptlyNotFoundReportsFinished pins the other
// half of NB2 (Opus review of PR #864, 2nd round): when Adopt answers
// ok=false with ctx still healthy (no deadline pressure at all — the
// backend genuinely has no notion of this runtimeID), Subscribe must still
// report finished=true — the container really is gone (or never existed),
// so a WS/SSE caller reporting "job done" here is correct, not a false
// positive. This is the regression guard alongside
// TestRunner_Subscribe_HangingAdoptHitsDeadline just above, which pins the
// opposite (ctx-exhausted) case.
func TestRunner_Subscribe_AdoptPromptlyNotFoundReportsFinished(t *testing.T) {
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &promptlyNotFoundBackend{}}

	_, _, _, ok, finished := r.Subscribe(jobID)
	if ok {
		t.Fatal("want ok=false: promptlyNotFoundBackend.Adopt always returns false")
	}
	if !finished {
		t.Error("want finished=true: Adopt found nothing with ctx still healthy — the container is genuinely gone, not merely unreachable")
	}
}

// TestRunner_Subscribe_NoRuntimeIDYetButJobRowExists_ReportsNotFinished
// pins the "no runtime_id yet" half of NB2: a job row that exists but has
// not been given a runtime_id yet (the window between CreateJob and
// launchSandbox's own UpdateJob call — see Subscribe's own doc comment)
// must NOT be reported as finished. The job may still be about to launch
// (BuildSandboxSpec / workspace-home resolution / init can take real
// wall-clock time — PR #861 added a debug log specifically because that
// step can be slow), so finished=true here would tell a caller landing in
// this exact window that a job which hasn't even started yet is already
// done.
func TestRunner_Subscribe_NoRuntimeIDYetButJobRowExists_ReportsNotFinished(t *testing.T) {
	// runtimeID "" models a job row CreateJob has already persisted but
	// launchSandbox has not yet reached its own `job.RuntimeID = handleID`
	// UpdateJob call for.
	dbConn, jobID := newRunnerTestDBWithJob(t, "")
	r := &Runner{DB: dbConn, Backend: &promptlyNotFoundBackend{}}

	_, _, _, ok, finished := r.Subscribe(jobID)
	if ok {
		t.Fatal("want ok=false: the job has no runtime_id yet")
	}
	if finished {
		t.Error("want finished=false: the job row exists and may still be about to launch, not already done")
	}
}

// TestRunner_Subscribe_UnknownJobID_ReportsFinished is the regression guard
// alongside the test just above: a jobID with NO row at all (typo'd, or a
// job old enough to have been GC'd) will never resolve, so finished=true
// (a clean, terminating answer) remains correct — the pre-NB2 behavior for
// this specific sub-case was already right, and must stay right now that
// the OTHER sub-case (row exists, no runtime_id yet) no longer shares its
// hardcoded value.
func TestRunner_Subscribe_UnknownJobID_ReportsFinished(t *testing.T) {
	dbConn, _ := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &promptlyNotFoundBackend{}}

	_, _, _, ok, finished := r.Subscribe("no-such-job-id")
	if ok {
		t.Fatal("want ok=false: no job row matches this jobID")
	}
	if !finished {
		t.Error("want finished=true: a jobID with no row at all will never resolve")
	}
}

// TestRunner_Subscribe_PreLaunchFailedJob_ReportsFinished pins NB3 (Opus
// review of PR #864, round 3): a job that failed BEFORE ever reaching
// Launch — Runner.failJob (runner.go, 8 call sites: spec build /
// workspace-home resolution / host-command resolution, ...) persists a
// TERMINAL "failed" status row with runtime_id left permanently empty —
// must report finished=true, not finished=false.
//
// An earlier version of the !found branch's finished logic
// (jobRowExists, since replaced by jobFinishedBeforeRuntime) checked only
// whether a jobs row existed at all, which cannot tell "still might
// launch" (CreateJob's default status="running", no runtime_id YET) apart
// from "will never launch" (status="failed", no runtime_id EVER) — both
// have the exact same "row exists, no runtime_id" shape. Reported as
// finished=false, the operator-visible consequence was `boid job attach`
// permanently erroring "job is still running but has no live output
// stream right now; try again" (ws_attach.go) and the Web UI job page
// permanently showing "Live output is temporarily unavailable ... Reload
// the page to retry" (job_log_sse.go) for a job that will truly never run
// again.
func TestRunner_Subscribe_PreLaunchFailedJob_ReportsFinished(t *testing.T) {
	dbConn, jobID := newRunnerTestDBWithJob(t, "")
	job, err := GetJob(dbConn, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	// Models Runner.failJob's own persisted shape: terminal status, no
	// runtime_id, ever.
	job.Status = JobStatusFailed
	job.Output = "spec build failed: boom"
	if err := UpdateJob(dbConn, job); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}

	r := &Runner{DB: dbConn, Backend: &promptlyNotFoundBackend{}}

	_, _, _, ok, finished := r.Subscribe(jobID)
	if ok {
		t.Fatal("want ok=false: the job never got a runtime_id")
	}
	if !finished {
		t.Error("want finished=true: a job in terminal status \"failed\" with no runtime_id will never launch, let alone stream")
	}
}

// TestRunner_WriteInput_HangingAdoptHitsDeadline is WriteInput's sibling of
// the Subscribe test above — goroutine + select for the same reason.
func TestRunner_WriteInput_HangingAdoptHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &hangingAdoptBackend{}}

	done := make(chan error, 1)
	go func() { done <- r.WriteInput(jobID, []byte("hello")) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error: Adopt never resolved before the deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WriteInput did not return within 5s against an Adopt whose engine call never answers")
	}
}

// TestRunner_ResizeRuntime_HangingAdoptHitsDeadline is ResizeRuntime's
// sibling — the WS "resize" frame ingress this PR's own body originally
// (and incorrectly) claimed was already fully fixed via
// containerSession.Resize alone.
func TestRunner_ResizeRuntime_HangingAdoptHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &hangingAdoptBackend{}}

	done := make(chan error, 1)
	go func() { done <- r.ResizeRuntime(jobID, TerminalSize{Rows: 24, Cols: 80}) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error: Adopt never resolved before the deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResizeRuntime did not return within 5s against an Adopt whose engine call never answers; a WS resize frame would hang the connection")
	}
}

// TestRunner_CloseInput_HangingAdoptHitsDeadline is CloseInput's sibling.
func TestRunner_CloseInput_HangingAdoptHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &hangingAdoptBackend{}}

	done := make(chan error, 1)
	go func() { done <- r.CloseInput(jobID) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error: Adopt never resolved before the deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseInput did not return within 5s against an Adopt whose engine call never answers")
	}
}

// --- Runner.StopJobRuntime / Runner.SignalJobRuntime ---------------------

// hangingControlSession is a backend.SandboxSession whose Stop/Signal block
// until the context they are given is done — modelling a wedged
// docker/podman engine socket that accepts the request and never answers.
// Subscribe/WriteInput/CloseInput/Wait are not exercised by the tests in
// this file and return ErrRuntimeUnsupported/zero values. Resize returns
// resizeErr verbatim (default nil) — TestRunner_ResizeRuntimeID_
// DeadlineErrorIsWrapped below sets it to context.DeadlineExceeded to model
// what containerSession.Resize's own internal timeout produces.
type hangingControlSession struct {
	id        string
	resizeErr error
}

var _ backend.SandboxSession = (*hangingControlSession)(nil)

func (s *hangingControlSession) ID() string { return s.id }
func (s *hangingControlSession) Subscribe() (RuntimeSnapshot, <-chan []byte, func(), bool, bool) {
	return RuntimeSnapshot{}, nil, func() {}, false, true
}
func (s *hangingControlSession) WriteInput([]byte) error { return ErrRuntimeUnsupported }
func (s *hangingControlSession) CloseInput() error       { return ErrRuntimeUnsupported }
func (s *hangingControlSession) Resize(backend.TerminalSize) error {
	return s.resizeErr
}
func (s *hangingControlSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return backend.RuntimeExit{}, ErrRuntimeUnsupported
}
func (s *hangingControlSession) Stop(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (s *hangingControlSession) Signal(ctx context.Context, _ syscall.Signal) error {
	<-ctx.Done()
	return ctx.Err()
}

// hangingControlBackend is a backend.SandboxBackend whose Adopt hands back
// a pre-configured session immediately (this file's Stop/Signal/Resize
// tests target what happens AFTER Adopt resolves — Adopt's own ctx
// propagation, including the in-flight-join gap Major 1 fixed, is pinned
// separately by TestContainerBackend_Adopt_InFlightJoinRespectsContextDeadline
// and the Hanging*AdoptHitsDeadline tests above) and whose Launch, when
// sess is set, "starts" that same session successfully —
// TestRunner_LaunchSandbox_PersistFailureStopsSessionUnderDeadline below
// needs a Launch that succeeds so launchSandbox reaches its
// post-UpdateJob-failure Stop call.
type hangingControlBackend struct {
	sess backend.SandboxSession
}

var _ backend.SandboxBackend = (*hangingControlBackend)(nil)

func (b *hangingControlBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	if b.sess == nil {
		return nil, fmt.Errorf("hangingControlBackend.Launch: no session configured")
	}
	return b.sess, nil
}
func (b *hangingControlBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return b.sess, true
}
func (b *hangingControlBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// TestRunner_StopJobRuntime_HangingEngineHitsDeadline pins the runner.go
// StopJobRuntime fix: a `boid task stop`/task-abort call must not hang
// forever when the adopted session's Stop blocks against a wedged engine.
func TestRunner_StopJobRuntime_HangingEngineHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)

	sess := &hangingControlSession{id: "runtime-xyz"}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	start := time.Now()
	r.StopJobRuntime("runtime-xyz")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("StopJobRuntime took %v against an engine that never answers; `boid task stop` would hang", elapsed)
	}
}

// TestRunner_StopJobRuntime_TimeoutWarnsLoudly pins Moderate 4: when the
// control-call deadline fires, StopJobRuntime must say so loudly (Warn),
// not silently (Debug) — the caller (finalizeTerminal → CleanupTaskWindow)
// is synchronous with no return value, so an operator grepping boid.log is
// the only way to learn the container may still be running.
func TestRunner_StopJobRuntime_TimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)

	sess := &hangingControlSession{id: "runtime-xyz"}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	r.StopJobRuntime("runtime-xyz")

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("StopJobRuntime timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// TestRunner_StopJobRuntime_AdoptTimeoutWarnsLoudly pins the OTHER half of
// Moderate 4 — the `!ok` branch, not session.Stop's — which
// TestRunner_StopJobRuntime_TimeoutWarnsLoudly above cannot reach: that
// test's hangingControlBackend.Adopt returns a session immediately, so
// StopJobRuntime always falls through to the session.Stop timeout branch
// and never touches the `!ok` + ctx.Err() != nil Warn a few lines above it.
// Deleting that whole block still left every existing test (this file
// included) green — go test ./internal/dispatcher/... passed unchanged —
// which is exactly the coverage gap this test closes. It matters more after
// Major 1/2: an in-flight-join timeout or a cache-miss ContainerInspect
// timeout now resolve THROUGH Adopt returning ok=false, not through
// session.Stop, so this is the more commonly hit of the two paths, not a
// corner case.
func TestRunner_StopJobRuntime_AdoptTimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)

	r := &Runner{Backend: &hangingAdoptBackend{}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.StopJobRuntime("runtime-xyz")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopJobRuntime did not return within 5s against an Adopt whose engine call never answers; want it bounded by sessionControlCallTimeout")
	}

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("StopJobRuntime's Adopt timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// TestRunner_SignalJobRuntime_HangingEngineHitsDeadline is the Signal
// sibling of the Stop test above: `boid agent stop`'s SIGUSR1 delivery
// (NotifyTask → SignalJobRuntime) must not hang forever either.
func TestRunner_SignalJobRuntime_HangingEngineHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)

	sess := &hangingControlSession{id: "runtime-xyz"}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	start := time.Now()
	r.SignalJobRuntime("runtime-xyz", syscall.SIGUSR1)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("SignalJobRuntime took %v against an engine that never answers; `boid agent stop` would hang", elapsed)
	}
}

// TestRunner_SignalJobRuntime_TimeoutWarnsLoudly is SignalJobRuntime's
// sibling of TestRunner_StopJobRuntime_TimeoutWarnsLoudly (Moderate 4).
func TestRunner_SignalJobRuntime_TimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)

	sess := &hangingControlSession{id: "runtime-xyz"}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	r.SignalJobRuntime("runtime-xyz", syscall.SIGUSR1)

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("SignalJobRuntime timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
}

// TestRunner_SignalJobRuntime_AdoptTimeoutWarnsLoudly is
// TestRunner_StopJobRuntime_AdoptTimeoutWarnsLoudly's Signal sibling — the
// `!ok` + ctx.Err() != nil Warn branch in SignalJobRuntime, which
// TestRunner_SignalJobRuntime_TimeoutWarnsLoudly above never reaches for
// the same reason (its hangingControlBackend.Adopt never times out).
func TestRunner_SignalJobRuntime_AdoptTimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)

	r := &Runner{Backend: &hangingAdoptBackend{}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.SignalJobRuntime("runtime-xyz", syscall.SIGUSR1)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SignalJobRuntime did not return within 5s against an Adopt whose engine call never answers; want it bounded by sessionControlCallTimeout")
	}

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("SignalJobRuntime's Adopt timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// --- Runner.ResizeRuntimeID's error wrapping (Minor 6) --------------------

// TestRunner_ResizeRuntimeID_DeadlineErrorIsWrapped pins Minor 6: the moby
// client hands context errors back UNDECORATED (Adopt/Stop/Signal's own
// callers rely on this — see this test file's other errors.Is assertions),
// so a raw context.DeadlineExceeded from session.Resize must not reach the
// HTTP resize route's caller verbatim (it would read as
// `context deadline exceeded` with nothing naming an unresponsive engine as
// the cause).
func TestRunner_ResizeRuntimeID_DeadlineErrorIsWrapped(t *testing.T) {
	sess := &hangingControlSession{id: "runtime-xyz", resizeErr: context.DeadlineExceeded}
	r := &Runner{Backend: &hangingControlBackend{sess: sess}}

	err := r.ResizeRuntimeID(context.Background(), "runtime-xyz", TerminalSize{Rows: 24, Cols: 80})

	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v no longer wraps context.DeadlineExceeded", err)
	}
	if err.Error() == context.DeadlineExceeded.Error() {
		t.Errorf("error %q is the bare context error, not wrapped with engine-unresponsive context", err)
	}
}

// TestRunner_ResizeRuntimeID_AdoptTimeoutIsWrapped pins the Moderate 4 /
// Minor 6 follow-up (Opus review of PR #857, 2nd round): when Adopt itself
// — not session.Resize — is the one that hits the deadline, ResizeRuntimeID
// must not return the generic ErrRuntimeUnsupported (indistinguishable from
// "this runtimeID legitimately doesn't support attach"); it must say the
// engine did not respond, the same as the sibling case
// TestRunner_ResizeRuntimeID_DeadlineErrorIsWrapped pins for session.Resize.
func TestRunner_ResizeRuntimeID_AdoptTimeoutIsWrapped(t *testing.T) {
	r := &Runner{Backend: &hangingAdoptBackend{}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := r.ResizeRuntimeID(ctx, "runtime-xyz", TerminalSize{Rows: 24, Cols: 80})

	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("error %v is ErrRuntimeUnsupported; an Adopt timeout must be distinguished from a normal unsupported-runtime result", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
}

// --- Runner.ResizeRuntimeID's deadline floor (next-session-container-
// backend-followups.md #1, Opus review of PR #862) ------------------------

// TestRunner_ResizeRuntimeID_UnboundedCallerContextHitsDeadline pins the
// runner.go ResizeRuntimeID fix, the exact defect PR #862 fixed one function
// higher up in this same file for CanAttach and left in place here: ctx went
// into Adopt completely unbounded. ResizeRuntimeID's only caller
// (internal/server/job_runtime_routes.go's POST /api/jobs/{id}/resize)
// passes req.Context(), and the daemon's own http.Server sets neither
// ReadTimeout nor WriteTimeout (internal/server/server.go), so a client that
// stays connected gives ResizeRuntimeID an effectively unbounded ctx.
//
// As with CanAttach, this does not hang ResizeRuntimeID itself — Adopt's
// in-flight join already selects on ctx (Major 1, PR #857). What it does is
// let an unbounded ResizeRuntimeID sit as the in-flight attempt's OWNER for
// that runtimeID against a wedged engine, so every bounded joiner for the
// SAME runtimeID (StopJobRuntime, SignalJobRuntime, ...) burns its whole
// sessionControlCallTimeout budget waiting on that owner instead of getting
// a real answer: a `boid task stop` that fails every single time.
//
// Runs ResizeRuntimeID in a goroutine and asserts via select/time.After for
// the reason spelled out on TestRunner_CanAttach_
// UnboundedCallerContextHitsDeadline: called inline, a regression that drops
// the floor does not fail this test, it HANGS it until go test's own
// -timeout kills the package with a goroutine dump.
func TestRunner_ResizeRuntimeID_UnboundedCallerContextHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	done := make(chan error, 1)
	go func() {
		done <- r.ResizeRuntimeID(context.Background(), "runtime-xyz", TerminalSize{Rows: 24, Cols: 80})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error: Adopt never resolved before the deadline")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResizeRuntimeID did not return within 5s against an unbounded caller ctx and an Adopt whose engine call never answers; want it bounded by sessionControlCallTimeout")
	}
}

// TestRunner_ResizeRuntimeID_CallerShorterDeadlineWins pins that
// sessionControlCallTimeout is a FLOOR under the caller's own ctx, not a
// fixed replacement for it — the sibling of
// TestRunner_CanAttach_CallerShorterDeadlineWins. The floor is set larger
// than the caller's deadline here, so a pass is only possible if the
// caller's own, shorter deadline actually governs.
func TestRunner_ResizeRuntimeID_CallerShorterDeadlineWins(t *testing.T) {
	withSessionControlCallTimeout(t, 5*time.Second)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := r.ResizeRuntimeID(ctx, "runtime-xyz", TerminalSize{Rows: 24, Cols: 80})
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("ResizeRuntimeID took %v; want it bounded by the caller's own ~100ms deadline, not sessionControlCallTimeout's 5s floor", elapsed)
	}
	if err == nil {
		t.Fatal("want an error: Adopt never resolved before the caller's own deadline")
	}
}

// TestRunner_ResizeRuntimeID_TimeoutWarnsLoudly pins the operator-visibility
// half of this fix: ResizeRuntimeID's caller turns its error into a plain
// HTTP 400 body (job_runtime_routes.go), which is not in boid.log at all, so
// an operator grepping the daemon log for "why did resize stop working"
// sees nothing unless ResizeRuntimeID itself says so — the same argument
// StopJobRuntime/SignalJobRuntime/CanAttach's own Warns already make.
func TestRunner_ResizeRuntimeID_TimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	done := make(chan error, 1)
	go func() {
		done <- r.ResizeRuntimeID(context.Background(), "runtime-xyz", TerminalSize{Rows: 24, Cols: 80})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error: Adopt never resolved before the deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResizeRuntimeID did not return within 5s; want it bounded by sessionControlCallTimeout")
	}

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("ResizeRuntimeID's Adopt timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// TestRunner_ResizeRuntimeID_CallerCancelIsNotReportedAsEngineTimeout pins
// the two-causes split PR #862 made for CanAttach and this function was left
// without. ctx here is derived from the CALLER's ctx, so ctx.Err() after a
// false Adopt can be Canceled (the HTTP client disconnected mid-request —
// ordinary and non-actionable) just as easily as DeadlineExceeded (the floor
// fired — the engine may be wedged). Pre-fix, both produced the identical
// `engine did not respond in time: context canceled` error, which names the
// engine for something the engine had nothing to do with, and logged
// nothing at all.
//
// Two assertions, matching TestRunner_CanAttach_CancelLogsDebugNotWarn: the
// cancellation must not be logged at WARN (it is not evidence of a wedged
// engine), and the returned error must not blame the engine's response time.
func TestRunner_ResizeRuntimeID_CallerCancelIsNotReportedAsEngineTimeout(t *testing.T) {
	withSessionControlCallTimeout(t, 5*time.Second)
	buf := captureLogs(t, slog.LevelDebug)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.ResizeRuntimeID(ctx, "runtime-xyz", TerminalSize{Rows: 24, Cols: 80})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ResizeRuntimeID did not return promptly after its caller's ctx was canceled")
	}

	if err == nil {
		t.Fatal("want an error: Adopt never resolved before cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
	if strings.Contains(err.Error(), "did not respond in time") {
		t.Errorf("error %q blames the engine's response time for what is an ordinary client disconnect", err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("ResizeRuntimeID logged at WARN for an ordinary client cancellation; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Errorf("ResizeRuntimeID logged nothing at DEBUG for the canceled case; log was:\n%s", buf.String())
	}
}

// TestRunner_ResizeRuntimeID_NotFoundDoesNotWarn is the regression guard for
// the two tests above, sibling of TestRunner_CanAttach_NotFoundDoesNotWarn:
// the ordinary "no such runtime" result (Adopt resolves immediately to
// ok=false, ctx never expires) must stay silent at WARN and must keep
// returning ErrRuntimeUnsupported, not an engine-blaming error.
func TestRunner_ResizeRuntimeID_NotFoundDoesNotWarn(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	r := &Runner{Backend: &notAdoptableBackend{}}

	err := r.ResizeRuntimeID(context.Background(), "runtime-xyz", TerminalSize{Rows: 24, Cols: 80})

	if !errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("want ErrRuntimeUnsupported for a legitimate not-found Adopt result, got %v", err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("ResizeRuntimeID logged a WARN for a legitimate not-found Adopt result; log was:\n%s", buf.String())
	}
}

// --- Runner.launchSandbox's post-persist-failure Stop ---------------------

// newClosedTestDB returns a migrated then immediately-closed *sql.DB, so any
// query against it (including UpdateJob's inspectJobColumns) fails
// deterministically with "sql: database is closed" — the mechanism this
// test uses to reach launchSandbox's cleanup-after-UpdateJob-failure branch
// without needing a real persistence fault to occur. Not testutil.NewTestDB:
// testutil transitively imports internal/server (which imports
// internal/dispatcher), so importing it from this internal-package test
// file (package dispatcher, not dispatcher_test) would be an import cycle —
// same reasoning as gitgateway_wire_test.go's newGatewayTestDB, which this
// mirrors.
func newClosedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := d.Conn.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	return d.Conn
}

// TestRunner_LaunchSandbox_PersistFailureStopsSessionUnderDeadline pins the
// runner.go launchSandbox fix: when UpdateJob fails right after a
// successful Launch, the best-effort session.Stop cleanup it triggers must
// not be able to hang this dispatch call forever against a wedged engine.
func TestRunner_LaunchSandbox_PersistFailureStopsSessionUnderDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)

	sess := &hangingControlSession{id: "runtime-persist-fail"}
	r := &Runner{
		DB:      newClosedTestDB(t),
		Backend: &hangingControlBackend{sess: sess},
	}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "h", Role: string(orchestrator.JobKindHook)}

	start := time.Now()
	_, err := r.launchSandbox(context.Background(), launchSandboxInput{
		Job:  job,
		Spec: sandbox.Spec{ID: "job-1", Argv: []string{"true"}},
	})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("launchSandbox took %v against an engine that never answers; the dispatch call would hang", elapsed)
	}
	if err == nil {
		t.Fatal("want an error: UpdateJob was forced to fail against a closed DB")
	}
}

// --- Runner.CanAttach's deadline floor (next-session-container-backend-
// followups.md #2, Opus review of PR #857) --------------------------------

// TestRunner_CanAttach_UnboundedCallerContextHitsDeadline pins the runner.go
// CanAttach fix: before it, ctx went into Adopt completely unbounded —
// CanAttach's only caller (internal/server/job_runtime_routes.go's
// resolveAttachableJob) passes req.Context(), so a client that stayed
// connected gave CanAttach an effectively unbounded ctx. Adopt's in-flight
// join already selects on ctx (Major 1 above), so this did not itself hang
// — but an unbounded CanAttach caller could sit as the in-flight attempt's
// "owner" against a wedged engine, and every bounded joiner for the SAME
// runtimeID (StopJobRuntime, SignalJobRuntime, ...) would then exhaust its
// own sessionControlCallTimeout budget waiting on that unbounded owner
// instead of ever getting a real answer from the engine — a `boid task
// stop` that fails every single time, not hangs.
//
// [NB-3, Opus independent review of this PR]: runs CanAttach in a goroutine
// and asserts via select/time.After, the same pattern
// TestRunner_CanAttach_CallerCancelPropagates below already uses, rather
// than calling it inline and checking elapsed afterward. Inline, a mutation
// that drops the floor entirely does not fail this test — it HANGS it: the
// call below never returns, so t.Fatalf on the elapsed check is never
// reached, and the whole test binary sits until go test's own (much longer,
// often CI-default) -timeout kills it with a goroutine dump instead of a
// clean assertion failure. The select-based form always terminates this
// test's own goroutine within a bounded wait, pass or fail.
func TestRunner_CanAttach_UnboundedCallerContextHitsDeadline(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- r.CanAttach(context.Background(), "runtime-xyz") }()

	select {
	case ok := <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("CanAttach took %v against an unbounded caller ctx and an Adopt whose engine call never answers", elapsed)
		}
		if ok {
			t.Fatal("want false: Adopt never resolved before the deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CanAttach did not return within 5s against an unbounded caller ctx and an Adopt whose engine call never answers; want it bounded by sessionControlCallTimeout")
	}
}

// TestRunner_CanAttach_CallerShorterDeadlineWins pins that
// sessionControlCallTimeout acts as a FLOOR under CanAttach's caller-supplied
// ctx, not a fixed replacement for it: when the caller's own ctx already
// carries a shorter deadline than sessionControlCallTimeout,
// context.WithTimeout(ctx, sessionControlCallTimeout.Get()) keeps whichever
// of the two fires first — but only because CanAttach derives its ctx FROM
// the caller's ctx. sessionControlCallTimeout is deliberately set larger
// than the caller's deadline here so a pass is only possible if the
// caller's own, shorter deadline actually governs.
func TestRunner_CanAttach_CallerShorterDeadlineWins(t *testing.T) {
	withSessionControlCallTimeout(t, 5*time.Second)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	ok := r.CanAttach(ctx, "runtime-xyz")
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("CanAttach took %v; want it bounded by the caller's own ~100ms deadline, not sessionControlCallTimeout's 5s floor", elapsed)
	}
	if ok {
		t.Fatal("want false: Adopt never resolved before the caller's own deadline")
	}
}

// TestRunner_CanAttach_CallerCancelPropagates pins that a caller's own
// cancellation — job_runtime_routes.go's resolveAttachableJob passes
// req.Context(), so an HTTP client disconnecting mid-request cancels this —
// still reaches Adopt once the floor is layered on top of ctx, the same as
// it did when ctx flowed straight through before this fix.
// sessionControlCallTimeout is set well above the cancellation delay below,
// so a prompt return is only possible via the caller's cancel, not the
// floor firing.
func TestRunner_CanAttach_CallerCancelPropagates(t *testing.T) {
	withSessionControlCallTimeout(t, 5*time.Second)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- r.CanAttach(ctx, "runtime-xyz") }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("want false: Adopt never resolved before cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CanAttach did not return promptly after its caller's ctx was canceled")
	}
}

// TestRunner_CanAttach_CancelLogsDebugNotWarn pins the log-level nit (Opus
// independent review of this PR, round 2): CanAttach's own doc comment
// argues that ctx.Err() == context.Canceled (the HTTP client disconnecting
// mid-request) is an ordinary, non-actionable event, not evidence of a
// wedged engine — so it must log at Debug, not Warn. Warn stays reserved
// for context.DeadlineExceeded (the floor firing, or the caller's own
// shorter deadline), which IS actionable the same way StopJobRuntime's
// Warn is. Before this fix, CanAttach logged Warn for both causes alike,
// contradicting its own comment's reasoning.
func TestRunner_CanAttach_CancelLogsDebugNotWarn(t *testing.T) {
	withSessionControlCallTimeout(t, 5*time.Second)
	buf := captureLogs(t, slog.LevelDebug)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- r.CanAttach(ctx, "runtime-xyz") }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("want false: Adopt never resolved before cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CanAttach did not return promptly after its caller's ctx was canceled")
	}

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("CanAttach logged at WARN for an ordinary client cancellation; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Errorf("CanAttach logged nothing at DEBUG for the canceled case; log was:\n%s", buf.String())
	}
}

// TestRunner_CanAttach_TimeoutWarnsLoudly pins NB-1 (Opus independent review
// of this PR): CanAttach was the one caller of Adopt among the five this PR
// and its predecessor (#857) bound that still logged nothing when its own
// deadline fired. The caller-visible symptom is a 409 "job runtime does not
// support attach" from job_runtime_routes.go's resolveAttachableJob — this
// PR still does not change that response (see the PR description for why),
// but an operator grepping boid.log for "why did this attach/resize get
// rejected" must be able to tell "the engine never answered" apart from the
// routine "no such runtime" result, the same as the four
// runtime_subscriber_export.go routes above already allow.
//
// [NB-3, Opus independent review of this PR, round 2]: runs CanAttach in a
// goroutine and asserts via select/time.After, the same as
// TestRunner_CanAttach_UnboundedCallerContextHitsDeadline above — this test
// makes the identical inline hangingAdoptBackend + context.Background()
// mistake round 2's fix to THAT test was meant to close, just ~70 lines
// further down in the same commit. A regression that removes CanAttach's
// floor entirely must fail this test cleanly too, not hang the whole
// package alongside it.
func TestRunner_CanAttach_TimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)
	r := &Runner{Backend: &hangingAdoptBackend{}}

	done := make(chan bool, 1)
	go func() { done <- r.CanAttach(context.Background(), "runtime-xyz") }()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("want false: Adopt never resolved before the deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CanAttach did not return within 5s; want it bounded by sessionControlCallTimeout")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("CanAttach's Adopt timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// TestRunner_CanAttach_NotFoundDoesNotWarn is
// TestRunner_CanAttach_TimeoutWarnsLoudly's regression guard, sibling of
// TestRunner_Subscribe_NotFoundDoesNotWarn below: the ordinary "no such
// runtime" result (Adopt resolves immediately to ok=false, ctx never
// expires) must stay silent at WARN.
func TestRunner_CanAttach_NotFoundDoesNotWarn(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	r := &Runner{Backend: &notAdoptableBackend{}}

	ok := r.CanAttach(context.Background(), "runtime-xyz")

	if ok {
		t.Fatal("want false")
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("CanAttach logged a WARN for a legitimate not-found Adopt result; log was:\n%s", buf.String())
	}
}

// --- runtime_subscriber_export.go Adopt-timeout visibility (next-session-
// container-backend-followups.md #3, Opus review of PR #857) -------------

// notAdoptableBackend is a backend.SandboxBackend whose Adopt reports
// ok=false immediately without ever touching ctx — modelling Adopt's most
// common false result: the runtimeID's job has already exited/been reaped,
// with no engine round-trip involved at all, let alone a slow one. Used by
// the regression-guard tests below: Subscribe/WriteInput/ResizeRuntime/
// CloseInput's new ctx.Err()-gated Warn logging and error wrapping must NOT
// fire for this ordinary case — an operator/API caller must still be able
// to tell "there is nothing here" apart from "the engine did not answer".
type notAdoptableBackend struct{}

var _ backend.SandboxBackend = (*notAdoptableBackend)(nil)

func (b *notAdoptableBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return nil, fmt.Errorf("notAdoptableBackend.Launch is not implemented")
}
func (b *notAdoptableBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}
func (b *notAdoptableBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// TestRunner_Subscribe_AdoptTimeoutWarnsLoudly pins #3 for Subscribe: when
// Adopt's control-call deadline fires, "the WS attach / SSE follow endpoint
// could not attach" must not look identical in the logs to "this job has no
// live output to attach to" (the routine, healthy case) — an operator
// grepping boid.log for a "terminal stayed blank" report needs to be able to
// tell the two apart.
//
// [NB-3, Opus independent review of this PR, round 2]: runs Subscribe in a
// goroutine and asserts via select/time.After rather than calling it
// inline — the same inline-hangingAdoptBackend-plus-unbounded-ctx shape
// flagged on TestRunner_CanAttach_TimeoutWarnsLoudly, just with the ctx
// constructed internally by Subscribe itself rather than passed in. A
// mutation that drops Subscribe's own context.WithTimeout wrapping would
// otherwise hang this test (and the whole package) instead of failing it
// cleanly.
//
// Also asserts finished=false in the SAME test (Opus review of PR #864,
// NB2 merge-conflict resolution): this is exactly the "Adopt's own
// deadline fired" case both the Warn log above and Subscribe's finished
// return exist to flag, so checking both together here — rather than only
// in a separate finished-focused test elsewhere in the package — pins
// that the ctx.Err()-gated Warn and the ctx.Err()-gated finished value
// can never drift apart: they read the exact same ctx.Err() != nil / == nil
// condition.
func TestRunner_Subscribe_AdoptTimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &hangingAdoptBackend{}}

	done := make(chan subscribeResult, 1)
	go func() {
		_, _, _, ok, finished := r.Subscribe(jobID)
		done <- subscribeResult{ok: ok, finished: finished}
	}()

	select {
	case res := <-done:
		if res.ok {
			t.Fatal("want ok=false: Adopt never resolved before the deadline")
		}
		if res.finished {
			t.Error("want finished=false: Adopt timing out against a wedged engine tells us nothing about whether the job has actually exited")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return within 5s; want it bounded by sessionControlCallTimeout")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("Subscribe's Adopt timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// TestRunner_Subscribe_NotFoundDoesNotWarn is
// TestRunner_Subscribe_AdoptTimeoutWarnsLoudly's regression guard: the
// ordinary "no runtime to subscribe to" result (Adopt resolves immediately
// to ok=false, ctx never expires) must stay silent at WARN — it is the
// routine, expected outcome for any job whose sandbox has already exited,
// not something an operator should be paged about. Also asserts
// finished=true (Opus review of PR #864, NB2 merge-conflict resolution):
// Adopt answering promptly with ctx still healthy means the container is
// genuinely gone, so reporting the job as done is correct here, not a
// false positive.
func TestRunner_Subscribe_NotFoundDoesNotWarn(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &notAdoptableBackend{}}

	_, _, _, ok, finished := r.Subscribe(jobID)

	if ok {
		t.Fatal("want ok=false")
	}
	if !finished {
		t.Error("want finished=true: Adopt found nothing with ctx still healthy — the container is genuinely gone, not merely unreachable")
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("Subscribe logged a WARN for a legitimate not-found Adopt result; log was:\n%s", buf.String())
	}
}

// TestRunner_WriteInput_AdoptTimeoutWarnsLoudly is Subscribe's sibling for
// WriteInput, which additionally returns an error to its caller: that error
// must name the engine as the cause (errors.Is context.DeadlineExceeded),
// not read as an ordinary ErrRuntimeUnsupported.
//
// [NB-3, Opus independent review of this PR, round 2]: goroutine + select,
// same reasoning as TestRunner_Subscribe_AdoptTimeoutWarnsLoudly above.
func TestRunner_WriteInput_AdoptTimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &hangingAdoptBackend{}}

	done := make(chan error, 1)
	go func() { done <- r.WriteInput(jobID, []byte("hello")) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WriteInput did not return within 5s; want it bounded by sessionControlCallTimeout")
	}

	if err == nil {
		t.Fatal("want an error: Adopt never resolved before the deadline")
	}
	if errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("error %v is ErrRuntimeUnsupported; an Adopt timeout must be distinguished from a normal unsupported-runtime result", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("WriteInput's Adopt timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// TestRunner_WriteInput_NotFoundDoesNotWarn is WriteInput's regression
// guard, sibling of TestRunner_Subscribe_NotFoundDoesNotWarn.
func TestRunner_WriteInput_NotFoundDoesNotWarn(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &notAdoptableBackend{}}

	err := r.WriteInput(jobID, []byte("hello"))

	if !errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("want ErrRuntimeUnsupported for a legitimate not-found Adopt result, got %v", err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("WriteInput logged a WARN for a legitimate not-found Adopt result; log was:\n%s", buf.String())
	}
}

// TestRunner_ResizeRuntime_AdoptTimeoutWarnsLoudly is Subscribe's sibling for
// ResizeRuntime — the WS attach transport's "resize" frame ingress.
//
// [NB-3, Opus independent review of this PR, round 2]: goroutine + select,
// same reasoning as TestRunner_Subscribe_AdoptTimeoutWarnsLoudly above.
func TestRunner_ResizeRuntime_AdoptTimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &hangingAdoptBackend{}}

	done := make(chan error, 1)
	go func() { done <- r.ResizeRuntime(jobID, TerminalSize{Rows: 24, Cols: 80}) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ResizeRuntime did not return within 5s; want it bounded by sessionControlCallTimeout")
	}

	if err == nil {
		t.Fatal("want an error: Adopt never resolved before the deadline")
	}
	if errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("error %v is ErrRuntimeUnsupported; an Adopt timeout must be distinguished from a normal unsupported-runtime result", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("ResizeRuntime's Adopt timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// TestRunner_ResizeRuntime_NotFoundDoesNotWarn is ResizeRuntime's regression
// guard, sibling of TestRunner_Subscribe_NotFoundDoesNotWarn.
func TestRunner_ResizeRuntime_NotFoundDoesNotWarn(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &notAdoptableBackend{}}

	err := r.ResizeRuntime(jobID, TerminalSize{Rows: 24, Cols: 80})

	if !errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("want ErrRuntimeUnsupported for a legitimate not-found Adopt result, got %v", err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("ResizeRuntime logged a WARN for a legitimate not-found Adopt result; log was:\n%s", buf.String())
	}
}

// TestRunner_CloseInput_AdoptTimeoutWarnsLoudly is Subscribe's sibling for
// CloseInput.
//
// [NB-3, Opus independent review of this PR, round 2]: goroutine + select,
// same reasoning as TestRunner_Subscribe_AdoptTimeoutWarnsLoudly above.
func TestRunner_CloseInput_AdoptTimeoutWarnsLoudly(t *testing.T) {
	withSessionControlCallTimeout(t, 200*time.Millisecond)
	buf := captureLogs(t, slog.LevelDebug)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &hangingAdoptBackend{}}

	done := make(chan error, 1)
	go func() { done <- r.CloseInput(jobID) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseInput did not return within 5s; want it bounded by sessionControlCallTimeout")
	}

	if err == nil {
		t.Fatal("want an error: Adopt never resolved before the deadline")
	}
	if errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("error %v is ErrRuntimeUnsupported; an Adopt timeout must be distinguished from a normal unsupported-runtime result", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("CloseInput's Adopt timing out logged nothing at WARN; log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "runtime-xyz") {
		t.Errorf("the warning does not name the runtime_id; log was:\n%s", buf.String())
	}
}

// TestRunner_CloseInput_NotFoundDoesNotWarn is CloseInput's regression
// guard, sibling of TestRunner_Subscribe_NotFoundDoesNotWarn.
func TestRunner_CloseInput_NotFoundDoesNotWarn(t *testing.T) {
	buf := captureLogs(t, slog.LevelDebug)
	dbConn, jobID := newRunnerTestDBWithJob(t, "runtime-xyz")
	r := &Runner{DB: dbConn, Backend: &notAdoptableBackend{}}

	err := r.CloseInput(jobID)

	if !errors.Is(err, ErrRuntimeUnsupported) {
		t.Errorf("want ErrRuntimeUnsupported for a legitimate not-found Adopt result, got %v", err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("CloseInput logged a WARN for a legitimate not-found Adopt result; log was:\n%s", buf.String())
	}
}
