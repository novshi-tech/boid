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
	return backend.ReapReport{}, nil
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
