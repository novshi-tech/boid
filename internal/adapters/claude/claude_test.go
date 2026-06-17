package claude

import (
	"context"
	"sync"
	"syscall"
	"testing"
)

// recordingSignaler captures SignalJobRuntime calls for assertions.
type recordingSignaler struct {
	mu      sync.Mutex
	runtime string
	signal  syscall.Signal
	count   int
}

func (r *recordingSignaler) SignalJobRuntime(runtimeID string, sig syscall.Signal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runtime = runtimeID
	r.signal = sig
	r.count++
}

func (r *recordingSignaler) recorded() (runtime string, sig syscall.Signal, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runtime, r.signal, r.count
}

func TestAdapter_StopAgent_DeliversSIGUSR1(t *testing.T) {
	sig := &recordingSignaler{}
	a := New(sig)

	if err := a.StopAgent(context.Background(), "rt-abc"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	runtime, signal, count := sig.recorded()
	if runtime != "rt-abc" {
		t.Errorf("runtime = %q, want rt-abc", runtime)
	}
	if signal != syscall.SIGUSR1 {
		t.Errorf("signal = %v, want SIGUSR1", signal)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (exactly one signal, no SIGKILL follow-up)", count)
	}
}

func TestAdapter_StopAgent_EmptyRuntimeID_NoOp(t *testing.T) {
	sig := &recordingSignaler{}
	a := New(sig)

	if err := a.StopAgent(context.Background(), ""); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	_, _, count := sig.recorded()
	if count != 0 {
		t.Errorf("count = %d, want 0 (empty runtimeID must be no-op)", count)
	}
}

func TestAdapter_StopAgent_NilLifecycle_NoOp(t *testing.T) {
	a := New(nil)
	if err := a.StopAgent(context.Background(), "rt-xyz"); err != nil {
		t.Fatalf("StopAgent with nil lifecycle: %v", err)
	}
}

func TestAdapter_ResumePayload_WithSessionID(t *testing.T) {
	a := New(nil)
	args, env := a.ResumePayload("sess-123")
	if len(args) != 2 || args[0] != "--resume" || args[1] != "sess-123" {
		t.Errorf("args = %v, want [--resume sess-123]", args)
	}
	if len(env) != 0 {
		t.Errorf("env = %v, want empty", env)
	}
}

func TestAdapter_ResumePayload_EmptySessionID(t *testing.T) {
	a := New(nil)
	args, env := a.ResumePayload("")
	if args != nil || env != nil {
		t.Errorf("expected nil args and env for empty session, got args=%v env=%v", args, env)
	}
}

func TestAdapter_StopSignalName_IsUSR1(t *testing.T) {
	a := New(nil)
	if got := a.StopSignalName(); got != "USR1" {
		t.Errorf("StopSignalName() = %q, want USR1", got)
	}
}

func TestAdapter_Interactive_AlwaysTrue(t *testing.T) {
	a := New(nil)
	if !a.Interactive() {
		t.Error("Interactive() = false, want true (claude always needs PTY)")
	}
}

func TestAdapter_SessionIDFromHookEnv(t *testing.T) {
	a := New(nil)
	env := map[string]string{
		"BOID_AGENT_SESSION_ID": "sess-abc",
		"OTHER_VAR":             "ignored",
	}
	if got := a.SessionIDFromHookEnv(env); got != "sess-abc" {
		t.Errorf("SessionIDFromHookEnv = %q, want sess-abc", got)
	}
}

func TestAdapter_SessionIDFromHookEnv_Missing(t *testing.T) {
	a := New(nil)
	if got := a.SessionIDFromHookEnv(map[string]string{}); got != "" {
		t.Errorf("SessionIDFromHookEnv with empty env = %q, want empty", got)
	}
}

func TestAdapter_Usage_ReturnsZeroStub(t *testing.T) {
	a := New(nil)
	u, err := a.Usage(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u.Model != "" || u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("Usage stub should return zero Usage, got %+v", u)
	}
}
