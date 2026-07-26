package dispatcher

import (
	"context"
	"fmt"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins Runner's attach/resize/signal ingress points end-to-end
// through the backend.SandboxBackend/SandboxSession seam (codex review
// Major 1 on PR #816): TestBoidBuiltinExecutor_AgentStop_SignalsRuntimeOnly
// stubs JobLifecycle and never reaches the backend/session layer at all, so
// it does not exercise the actual Runner.SignalJobRuntime →
// SandboxBackend.Adopt → SandboxSession.Signal chain. A fake backend
// (Runner.Backend, the same seam CanAttach/Adopt use in production) closes
// that gap here.

// wireFakeSession is a minimal backend.SandboxSession recording every
// Signal/Resize call it receives, keyed by the runtimeID Adopt was given.
type wireFakeSession struct {
	id string

	signalCalls []syscall.Signal
	resizeCalls []backend.TerminalSize
	stopCalls   int
}

var _ backend.SandboxSession = (*wireFakeSession)(nil)

func (s *wireFakeSession) ID() string { return s.id }
func (s *wireFakeSession) Subscribe() ([]byte, <-chan []byte, func(), bool) {
	return nil, nil, func() {}, false
}
func (s *wireFakeSession) WriteInput([]byte) error { return ErrRuntimeUnsupported }
func (s *wireFakeSession) CloseInput() error       { return ErrRuntimeUnsupported }
func (s *wireFakeSession) Resize(size backend.TerminalSize) error {
	s.resizeCalls = append(s.resizeCalls, size)
	return nil
}
func (s *wireFakeSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return backend.RuntimeExit{}, ErrRuntimeUnsupported
}
func (s *wireFakeSession) Stop(context.Context) error {
	s.stopCalls++
	return nil
}
func (s *wireFakeSession) Signal(_ context.Context, sig syscall.Signal) error {
	s.signalCalls = append(s.signalCalls, sig)
	return nil
}

// wireFakeBackend is a minimal backend.SandboxBackend recording every
// Adopt() call's runtimeID and, when adoptable is non-nil, handing back the
// same session every time (mirroring how a real backend would resolve
// repeat Adopt calls for the same live runtimeID).
type wireFakeBackend struct {
	adoptable  map[string]*wireFakeSession
	adoptCalls []string

	// reapReport/reapErr/reapCalls back TestRunner_ReapOrphans_DelegatesToBackend
	// — Runner.ReapOrphans (docs/plans/phase6-container-backend.md §PR7) is
	// a thin delegation to SandboxBackend.ReapOrphans, so this fake needs a
	// configurable return value rather than the fixed zero-value the other
	// tests in this file are content with.
	reapReport backend.ReapReport
	reapErr    error
	reapCalls  int
}

var _ backend.SandboxBackend = (*wireFakeBackend)(nil)

func (b *wireFakeBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return nil, fmt.Errorf("wireFakeBackend.Launch is not implemented — this fake only drives Adopt-based ingress")
}
func (b *wireFakeBackend) Adopt(_ context.Context, runtimeID string) (backend.SandboxSession, bool) {
	b.adoptCalls = append(b.adoptCalls, runtimeID)
	sess, ok := b.adoptable[runtimeID]
	if !ok {
		return nil, false
	}
	return sess, true
}

// RunWorkspaceInit satisfies WorkspaceInitExecutor, which
// resolveWorkspaceHome requires of every backend since PR5. Delegated to the
// same real-bash stand-in the workspace-home tests use
// (workspace_init_bash_backend_test.go), so a Dispatch through this fake
// leaves the home in the state production leaves it in.
func (b *wireFakeBackend) RunWorkspaceInit(ctx context.Context, req WorkspaceInitRequest) error {
	return (&bashWorkspaceInitBackend{}).RunWorkspaceInit(ctx, req)
}

func (b *wireFakeBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	b.reapCalls++
	return b.reapReport, b.reapErr
}

// TestRunner_SignalJobRuntime_RoutesThroughBackendAdoptToSessionSignal pins
// the full Runner.SignalJobRuntime → SandboxBackend.Adopt →
// SandboxSession.Signal chain end-to-end (codex review Major 1 on PR #816):
// `boid agent stop`'s SIGUSR1 delivery (NotifyTask → SignalJobRuntime) must
// actually reach a session's Signal method with the correct runtimeID and
// signal, not just a mocked-away JobLifecycle.
func TestRunner_SignalJobRuntime_RoutesThroughBackendAdoptToSessionSignal(t *testing.T) {
	sess := &wireFakeSession{id: "runtime-xyz"}
	be := &wireFakeBackend{adoptable: map[string]*wireFakeSession{"runtime-xyz": sess}}
	r := &Runner{Backend: be}

	r.SignalJobRuntime("runtime-xyz", syscall.SIGUSR1)

	if len(be.adoptCalls) != 1 || be.adoptCalls[0] != "runtime-xyz" {
		t.Fatalf("Adopt calls = %v, want exactly one call with runtimeID=runtime-xyz", be.adoptCalls)
	}
	if len(sess.signalCalls) != 1 || sess.signalCalls[0] != syscall.SIGUSR1 {
		t.Fatalf("session.Signal calls = %v, want exactly one SIGUSR1", sess.signalCalls)
	}
}

// TestRunner_SignalJobRuntime_UnadoptableRuntimeIDNeverReachesSession pins
// the companion negative case: when Adopt reports ok=false (unknown/stale
// runtimeID), SignalJobRuntime must not call Signal on anything.
func TestRunner_SignalJobRuntime_UnadoptableRuntimeIDNeverReachesSession(t *testing.T) {
	sess := &wireFakeSession{id: "runtime-xyz"}
	be := &wireFakeBackend{adoptable: map[string]*wireFakeSession{"runtime-xyz": sess}}
	r := &Runner{Backend: be}

	r.SignalJobRuntime("runtime-does-not-exist", syscall.SIGUSR1)

	if len(be.adoptCalls) != 1 || be.adoptCalls[0] != "runtime-does-not-exist" {
		t.Fatalf("Adopt calls = %v, want exactly one call with runtimeID=runtime-does-not-exist", be.adoptCalls)
	}
	if len(sess.signalCalls) != 0 {
		t.Fatalf("session.Signal calls = %v, want none (Adopt reported ok=false)", sess.signalCalls)
	}
}

// TestRunner_ResizeRuntimeID_RoutesThroughBackendAdoptToSessionResize is the
// resize-ingress sibling of the Signal wire test above, exercising the same
// Runner.Backend seam for ResizeRuntimeID (internal/server/
// job_runtime_routes.go's HTTP resize handler collapse point).
func TestRunner_ResizeRuntimeID_RoutesThroughBackendAdoptToSessionResize(t *testing.T) {
	sess := &wireFakeSession{id: "runtime-xyz"}
	be := &wireFakeBackend{adoptable: map[string]*wireFakeSession{"runtime-xyz": sess}}
	r := &Runner{Backend: be}

	size := TerminalSize{Rows: 50, Cols: 120}
	if err := r.ResizeRuntimeID(context.Background(), "runtime-xyz", size); err != nil {
		t.Fatalf("ResizeRuntimeID: %v", err)
	}

	if len(sess.resizeCalls) != 1 || sess.resizeCalls[0] != size {
		t.Fatalf("session.Resize calls = %v, want exactly one %+v", sess.resizeCalls, size)
	}
}

// TestRunner_ReapOrphans_DelegatesToBackend pins Runner.ReapOrphans
// (docs/plans/phase6-container-backend.md §PR7) as a pure delegation to
// r.sandboxBackend().ReapOrphans — internal/server/wire.go's startup
// sequence relies on getting the exact ReapReport/error the configured
// backend produced, unmodified, so it can compute which daemon_shutdown
// tasks to skip auto-reopening.
func TestRunner_ReapOrphans_DelegatesToBackend(t *testing.T) {
	want := backend.ReapReport{
		ReapedJobIDs: []string{"job-ok"},
		FailedJobIDs: []string{"job-bad"},
	}
	be := &wireFakeBackend{reapReport: want}
	r := &Runner{Backend: be}

	got, err := r.ReapOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if be.reapCalls != 1 {
		t.Fatalf("backend ReapOrphans calls = %d, want 1", be.reapCalls)
	}
	if len(got.ReapedJobIDs) != 1 || got.ReapedJobIDs[0] != "job-ok" {
		t.Errorf("ReapedJobIDs = %v, want [job-ok]", got.ReapedJobIDs)
	}
	if len(got.FailedJobIDs) != 1 || got.FailedJobIDs[0] != "job-bad" {
		t.Errorf("FailedJobIDs = %v, want [job-bad]", got.FailedJobIDs)
	}
}

// TestRunner_StopJobRuntime_RoutesThroughBackendAdoptToSessionStop pins
// [Blocker 3, PR7 codex review]: `boid task stop`/task abort's
// StopJobRuntime call must reach a session's Stop method via
// SandboxBackend.Adopt — not r.Runtime.Stop directly, which cannot stop a
// docker container when sandbox.backend: container is selected.
func TestRunner_StopJobRuntime_RoutesThroughBackendAdoptToSessionStop(t *testing.T) {
	sess := &wireFakeSession{id: "runtime-xyz"}
	be := &wireFakeBackend{adoptable: map[string]*wireFakeSession{"runtime-xyz": sess}}
	r := &Runner{Backend: be}

	r.StopJobRuntime("runtime-xyz")

	if len(be.adoptCalls) != 1 || be.adoptCalls[0] != "runtime-xyz" {
		t.Fatalf("Adopt calls = %v, want exactly one call with runtimeID=runtime-xyz", be.adoptCalls)
	}
	if sess.stopCalls != 1 {
		t.Fatalf("session.Stop calls = %d, want 1", sess.stopCalls)
	}
}

// TestRunner_StopJobRuntime_UnadoptableRuntimeIDNeverReachesSession pins the
// companion negative case: an unknown/stale runtimeID must not panic and
// must never reach Stop on anything.
func TestRunner_StopJobRuntime_UnadoptableRuntimeIDNeverReachesSession(t *testing.T) {
	sess := &wireFakeSession{id: "runtime-xyz"}
	be := &wireFakeBackend{adoptable: map[string]*wireFakeSession{"runtime-xyz": sess}}
	r := &Runner{Backend: be}

	r.StopJobRuntime("runtime-does-not-exist")

	if sess.stopCalls != 0 {
		t.Fatalf("session.Stop calls = %d, want 0 (Adopt reported ok=false)", sess.stopCalls)
	}
}

// TestRunner_CleanupTaskWindow_RoutesThroughBackendAdoptToSessionStop pins
// the task-abort sibling of the StopJobRuntime fix above: every runtime ID
// tracked for a task (trackTaskRuntime) must be stopped via the same
// Adopt→Stop seam, so `boid task stop`/abort on a container-backend task
// actually terminates the container instead of silently no-op'ing.
func TestRunner_CleanupTaskWindow_RoutesThroughBackendAdoptToSessionStop(t *testing.T) {
	sess1 := &wireFakeSession{id: "runtime-1"}
	sess2 := &wireFakeSession{id: "runtime-2"}
	be := &wireFakeBackend{adoptable: map[string]*wireFakeSession{
		"runtime-1": sess1,
		"runtime-2": sess2,
	}}
	r := &Runner{Backend: be}
	r.trackTaskRuntime("task-1", "runtime-1")
	r.trackTaskRuntime("task-1", "runtime-2")

	r.CleanupTaskWindow("task-1")

	if sess1.stopCalls != 1 {
		t.Errorf("runtime-1 session.Stop calls = %d, want 1", sess1.stopCalls)
	}
	if sess2.stopCalls != 1 {
		t.Errorf("runtime-2 session.Stop calls = %d, want 1", sess2.stopCalls)
	}
	// takeTaskRuntimes drains the tracked set — a second call must find
	// nothing left to stop.
	if remaining := r.takeTaskRuntimes("task-1"); len(remaining) != 0 {
		t.Errorf("takeTaskRuntimes after CleanupTaskWindow = %v, want empty (already drained)", remaining)
	}
}
