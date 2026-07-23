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
// Major 1 on PR #816): TestUsernsSession_Signal and
// TestBoidBuiltinExecutor_AgentStop_SignalsRuntimeOnly each stop short of
// Runner.SignalJobRuntime itself — the former constructs a usernsSession
// directly, the latter stubs JobLifecycle and never reaches the
// backend/session layer at all. Neither exercises the actual
// Runner.SignalJobRuntime → SandboxBackend.Adopt → SandboxSession.Signal
// chain. A fake backend (Runner.Backend, the same seam CanAttach/Adopt use
// in production) closes that gap here.

// wireFakeSession is a minimal backend.SandboxSession recording every
// Signal/Resize call it receives, keyed by the runtimeID Adopt was given.
type wireFakeSession struct {
	id string

	signalCalls []syscall.Signal
	resizeCalls []backend.TerminalSize
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
func (s *wireFakeSession) Stop(context.Context) error { return nil }
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
	r := &Runner{Runtime: &ubFakeRuntime{}, Backend: be}

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
	r := &Runner{Runtime: &ubFakeRuntime{}, Backend: be}

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
	r := &Runner{Runtime: &ubFakeRuntime{}, Backend: be}

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
	r := &Runner{Runtime: &ubFakeRuntime{}, Backend: be}

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

// TestRunner_ReapOrphans_UsesUsernsStubByDefault pins that a Runner with no
// Backend override (the production default, absent config sandbox.backend:
// container) still gets a nil error and a zero ReapReport back — the
// userns backend's ReapOrphans no-op stub — rather than a nil-pointer
// panic. internal/server/wire.go calls this unconditionally on every
// daemon startup, so it must never blow up when the userns backend (every
// pre-PR7 deployment) is in play.
func TestRunner_ReapOrphans_UsesUsernsStubByDefault(t *testing.T) {
	r := &Runner{Runtime: &ubFakeRuntime{}}

	got, err := r.ReapOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(got.ReapedJobIDs) != 0 || len(got.FailedJobIDs) != 0 || got.GlobalError != nil {
		t.Errorf("ReapReport = %+v, want the zero value (userns backend stub)", got)
	}
}
