package dispatcher_test

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins Runner.watchRuntime's (runner.go) handling of
// backend.RuntimeExit.EngineError — the follow-up to PR #855 (flagged in an
// Opus review of PR #855) that gives an ENGINE fault (ContainerWait itself
// failing, or the wait response carrying an engine-side error — see
// waitResponseEngineError's doc comment in
// container_backend_workspace_init.go) a way to reach job.Output, instead of
// being indistinguishable from a hook script that simply exited non-zero on
// its own. container_backend_test.go pins the lower layer (waitLoop setting
// EngineError on the RuntimeExit it returns from Wait); this file pins that
// watchRuntime, one layer up, actually reads that field and changes what it
// persists.

// engineExitSession is a minimal backend.SandboxSession whose Wait returns
// immediately with a caller-supplied backend.RuntimeExit — standing in for a
// real containerSession whose waitLoop has already run to completion.
type engineExitSession struct {
	id   string
	exit backend.RuntimeExit
}

var _ backend.SandboxSession = (*engineExitSession)(nil)

func (s *engineExitSession) ID() string { return s.id }

// Subscribe reports finished=true alongside ok=false: this fake models a
// session whose waitLoop has ALREADY run to completion (see this type's own
// doc comment) — a genuinely finished job, not a still-running one that
// merely lost its stream (the distinction backend.SandboxSession.Subscribe's
// own doc comment introduced, Opus review of PR #864, B2). Every test in
// this file drives Wait()/watchRuntime, never Subscribe(), so this value is
// never actually read by anything today; it is set correctly anyway so this
// fake keeps meaning what its own doc comment says if a future test in this
// file ever does exercise Subscribe.
func (s *engineExitSession) Subscribe() ([]byte, <-chan []byte, func(), bool, bool) {
	return nil, nil, func() {}, false, true
}
func (s *engineExitSession) WriteInput([]byte) error           { return nil }
func (s *engineExitSession) CloseInput() error                 { return nil }
func (s *engineExitSession) Resize(backend.TerminalSize) error { return nil }

// Wait returns immediately regardless of ctx — Runner.watchRuntime calls it
// with context.Background() in production, and this fake models a session
// whose underlying wait has ALREADY resolved (a real waitLoop having
// finished), not one still running.
func (s *engineExitSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return s.exit, nil
}
func (s *engineExitSession) Stop(context.Context) error                   { return nil }
func (s *engineExitSession) Signal(context.Context, syscall.Signal) error { return nil }

// engineExitBackend hands out engineExitSession values from Launch, and
// reuses this package's existing in-process workspace-home stand-ins
// (runtime_test_helpers_test.go's ensureWorkspaceHomeVolumeInProcess /
// runWorkspaceInitInProcess) so Dispatch's resolveWorkspaceHome step — which
// every Dispatch call reaches before a job ever gets to Launch — succeeds
// the same way statefulBackend's does.
type engineExitBackend struct {
	exit backend.RuntimeExit
}

var _ backend.SandboxBackend = (*engineExitBackend)(nil)

func (b *engineExitBackend) Launch(_ context.Context, _ sandbox.Spec, opts backend.LaunchOptions) (backend.SandboxSession, error) {
	id := opts.DesiredID
	if id == "" {
		id = "runtime-engine-exit-test"
	}
	return &engineExitSession{id: id, exit: b.exit}, nil
}
func (b *engineExitBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}
func (b *engineExitBackend) EnsureWorkspaceHomeVolume(_ context.Context, req dispatcher.WorkspaceHomeVolumeRequest) (string, error) {
	return ensureWorkspaceHomeVolumeInProcess(req)
}
func (b *engineExitBackend) RunWorkspaceInit(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	return runWorkspaceInitInProcess(ctx, req)
}
func (b *engineExitBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

func dispatchHookSpec() *orchestrator.JobSpec {
	return &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}
}

// TestWatchRuntime_EngineErrorSurfacesInJobOutput pins the fix itself: when
// the session's RuntimeExit carries a non-empty EngineError,
// Runner.watchRuntime's job.Output (persisted via UpdateJob, and delivered
// to r.CompleteJob's caller) must say so — both the engine's own message and
// that this is an ENGINE fault, not the job's own command failing — rather
// than the pre-fix "job runtime exited without boid job done (...)" wording,
// which reads identically whether a hook script exited 1 on purpose or the
// container engine itself never reported a status.
func TestWatchRuntime_EngineErrorSurfacesInJobOutput(t *testing.T) {
	r, d := newDispatchRunner(t)
	const engineMessage = "runtime wait failed: engine socket hung up"
	r.Backend = &engineExitBackend{exit: backend.RuntimeExit{ExitCode: 0, EngineError: engineMessage}}

	jobID, err := r.Dispatch(context.Background(), dispatchHookSpec(), nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.WaitForJobCtx(ctx, jobID)
	if err != nil {
		t.Fatalf("WaitForJobCtx: %v", err)
	}

	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
	if !strings.Contains(result.Output, engineMessage) {
		t.Errorf("Output = %q, does not contain the engine's own message %q", result.Output, engineMessage)
	}
	if !strings.Contains(strings.ToLower(result.Output), "engine") {
		t.Errorf("Output = %q, does not say the failure is an ENGINE fault (an operator cannot tell this from a hook script that exited 1 on its own)", result.Output)
	}

	// Read back from the DB, not just the in-memory CompleteJob result:
	// watchRuntime's UpdateJob call is what makes `boid job log` / job.Output
	// see this after the fact (CompleteJob's channel delivery is a
	// same-process convenience for in-flight waiters, not persistence).
	job, err := dispatcher.GetJob(d.Conn, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Output != result.Output {
		t.Errorf("persisted job.Output = %q, want it to match the CompleteJob result %q", job.Output, result.Output)
	}
	if job.ExitCode != 1 {
		t.Errorf("persisted job.ExitCode = %d, want 1", job.ExitCode)
	}
	if job.Status != dispatcher.JobStatusFailed {
		t.Errorf("persisted job.Status = %v, want failed", job.Status)
	}
}

// TestWatchRuntime_NoEngineErrorKeepsExistingOutputWording is the regression
// guard for requirement 4 (Opus review of PR #855): a job runtime that exits
// without calling `boid job done`, but WITHOUT an engine fault (EngineError
// empty — the ordinary "silent crash" case this path existed for before
// this fix), must keep producing the exact pre-fix wording. Existing
// operator tooling (log greps, docs) depends on this string; this test pins
// it byte for byte so a future change to the engine-error branch cannot
// accidentally widen and swallow this one too.
func TestWatchRuntime_NoEngineErrorKeepsExistingOutputWording(t *testing.T) {
	r, d := newDispatchRunner(t)
	r.Backend = &engineExitBackend{exit: backend.RuntimeExit{ExitCode: 3}}

	jobID, err := r.Dispatch(context.Background(), dispatchHookSpec(), nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// RuntimeID is persisted before watchRuntime's goroutine is even
	// started (launchSandbox's own UpdateJob call), so it is safe to read
	// back immediately — this is what the pre-fix wording embeds, and the
	// fake backend's generated ID is not hardcoded here on purpose.
	dispatchedJob, err := dispatcher.GetJob(d.Conn, jobID)
	if err != nil {
		t.Fatalf("GetJob (post-dispatch): %v", err)
	}
	runtimeID := dispatchedJob.RuntimeID
	if runtimeID == "" {
		t.Fatal("job.RuntimeID is empty right after Dispatch")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.WaitForJobCtx(ctx, jobID)
	if err != nil {
		t.Fatalf("WaitForJobCtx: %v", err)
	}

	wantOutput := fmt.Sprintf("job runtime exited without boid job done (runtime_id=%s, exit_code=%d)", runtimeID, 3)
	if result.Output != wantOutput {
		t.Errorf("Output = %q, want unchanged pre-fix wording %q (regression: the engine-error wording change must not touch this path)", result.Output, wantOutput)
	}
	if result.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3 (non-zero exit codes pass through unchanged)", result.ExitCode)
	}

	job, err := dispatcher.GetJob(d.Conn, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Output != wantOutput {
		t.Errorf("persisted job.Output = %q, want %q", job.Output, wantOutput)
	}
}
