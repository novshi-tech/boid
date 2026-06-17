// Package claude implements adapters.HarnessAdapter for Claude Code.
//
// Package placement: internal/adapters/claude/ (internal/adapters/ hosts the
// shared interface; each harness sub-package hosts its implementation).
//
// Stopping convention: SIGUSR1 is delivered to the runtime process group.
// run-agent.py intercepts it and forwards SIGTERM to the claude process only,
// leaving bash and the EXIT trap alive so payload_patch capture completes
// normally through the broker.
package claude

import (
	"context"
	"syscall"

	"github.com/novshi-tech/boid/internal/adapters"
)

// runtimeSignaler delivers a signal to a running runtime's process group.
// Satisfied by jobLifecycleAdapter (internal/server) and Runner (internal/dispatcher).
type runtimeSignaler interface {
	SignalJobRuntime(runtimeID string, sig syscall.Signal)
}

// Adapter implements adapters.HarnessAdapter for Claude Code.
type Adapter struct {
	lifecycle runtimeSignaler
}

// New returns a new Adapter backed by the given runtimeSignaler.
func New(lifecycle runtimeSignaler) *Adapter {
	return &Adapter{lifecycle: lifecycle}
}

// StopAgent delivers SIGUSR1 to the runtime's process group. run-agent.py
// handles it by sending SIGTERM to the claude subprocess only; bash and the
// EXIT trap survive so boid job done fires through the broker normally.
func (a *Adapter) StopAgent(_ context.Context, runtimeID string) error {
	if a.lifecycle == nil || runtimeID == "" {
		return nil
	}
	a.lifecycle.SignalJobRuntime(runtimeID, syscall.SIGUSR1)
	return nil
}

// ResumePayload returns the --resume flag for the given session ID. The caller
// passes these args to the start hook so claude resumes its prior session.
func (a *Adapter) ResumePayload(sessionID string) ([]string, map[string]string) {
	if sessionID == "" {
		return nil, nil
	}
	return []string{"--resume", sessionID}, nil
}

// Interactive returns true. Non-interactive claude --print invocations
// consume a separate Max credit pool that bills at a higher rate than PTY sessions.
func (a *Adapter) Interactive() bool {
	return true
}

// SessionIDFromHookEnv returns the BOID_AGENT_SESSION_ID variable from env.
// boid sets this variable from the awaiting trait when dispatching a resume hook.
func (a *Adapter) SessionIDFromHookEnv(env map[string]string) string {
	return env["BOID_AGENT_SESSION_ID"]
}

// Usage is not yet implemented. It will be wired in Phase 3 when the jobs
// table gains usage columns and the jsonl read path is finalised.
func (a *Adapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}
