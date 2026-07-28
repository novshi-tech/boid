//go:build linux

package dispatcher

import (
	"context"
	"fmt"
	"log/slog"
)

// RuntimeSubscriber subscribes to live output of a running job identified by jobID.
type RuntimeSubscriber interface {
	Subscribe(jobID string) (snapshot []byte, ch <-chan []byte, cancel func(), ok bool)
}

// RuntimeInputWriter provides write access to a running job's PTY input.
type RuntimeInputWriter interface {
	WriteInput(jobID string, data []byte) error
	ResizeRuntime(jobID string, size TerminalSize) error
	// CloseInput signals that no more input is coming for jobID — the WS
	// attach transport's counterpart to the old raw-hijack transport's
	// implicit half-close (docs/plans/cli-remote-connection.md Phase 3 PR3;
	// see LocalRuntime.CloseInputRuntime's doc comment for the full
	// rationale). A no-op for jobs whose runtime has no notion of "closing"
	// input (interactive PTY sessions, or non-interactive sessions with no
	// StdinForward pipe).
	CloseInput(jobID string) error
}

// runtimeIDForJob resolves jobID to its persisted runtime_id via the jobs
// table. found is false when the job has no row or no runtime_id yet.
func (r *Runner) runtimeIDForJob(jobID string) (runtimeID string, found bool) {
	if err := r.DB.QueryRow(`SELECT runtime_id FROM jobs WHERE id = ?`, jobID).Scan(&runtimeID); err != nil || runtimeID == "" {
		return "", false
	}
	return runtimeID, true
}

// Subscribe implements RuntimeSubscriber for Runner. It resolves jobID to a
// runtimeID via the jobs table, then adopts a SandboxSession for it
// (docs/plans/phase6-container-backend.md §PR1 — this is the "stream 1 本"
// seam shared by WS attach and the Web UI's SSE follow endpoint, both of
// which call through this same method).
//
// Adopt runs under sessionControlCallTimeout (runner.go — Opus review of
// PR #857, Major 2), not context.Background(): a
// cache-miss Adopt (fresh daemon restart, before anything has repopulated
// containerBackend's session cache — exactly the case Adopt's own doc
// comment names as its reason to exist) does a real `docker inspect`
// against the engine, and an unbounded context here would hang a WS attach
// or the Web UI's SSE follow request forever against a wedged engine.
//
// When Adopt's !adopted is caused by that same deadline firing (ctx.Err()
// != nil) rather than a legitimate "no such runtime" result, this logs a
// Warn naming the cause (next-session-container-backend-followups.md #3,
// Opus review of PR #857) — before this, a wedged engine and an
// already-exited job were indistinguishable to an operator reading the
// logs, both surfacing as silent ok=false, turning a "terminal stayed
// blank" bug report into a guessing game between "nothing to attach to"
// and "the engine never answered". Subscribe's signature has no error to
// carry the distinction into (unlike WriteInput/ResizeRuntime/CloseInput
// below), so the Warn log is the only place it can surface; ok stays false
// either way — the caller-visible contract is unchanged.
func (r *Runner) Subscribe(jobID string) (snapshot []byte, ch <-chan []byte, cancel func(), ok bool) {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return nil, nil, func() {}, false
	}
	ctx, cancelCtx := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancelCtx()
	session, adopted := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !adopted {
		if ctx.Err() != nil {
			slog.Warn("subscribe: adopt did not resolve before the control-call deadline; the runtime's live output cannot be attached to",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
		}
		return nil, nil, func() {}, false
	}
	return session.Subscribe()
}

// WriteInput implements RuntimeInputWriter for Runner. It resolves jobID to
// a runtimeID via the jobs table, then adopts a SandboxSession and writes
// through it. Adopt is bounded — see Subscribe's doc comment just above for
// why, including the ctx.Err()-gated Warn below.
//
// Unlike Subscribe, WriteInput has an error return, so an Adopt-deadline
// !adopted also gets a wrapped error distinguishing it from the ordinary
// ErrRuntimeUnsupported result (next-session-container-backend-followups.md
// #3, Opus review of PR #857) — the same "engine did not respond in time"
// phrasing ResizeRuntimeID (runner.go) already uses for session.Resize's own
// deadline error, so the two read as the same class of failure.
func (r *Runner) WriteInput(jobID string, data []byte) error {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return fmt.Errorf("runtime not found for job %s", jobID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancel()
	session, adopted := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !adopted {
		if ctx.Err() != nil {
			slog.Warn("write input: adopt did not resolve before the control-call deadline; input was not delivered",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
			return fmt.Errorf("write input: engine did not respond in time: %w", ctx.Err())
		}
		return ErrRuntimeUnsupported
	}
	return session.WriteInput(data)
}

// ResizeRuntime implements RuntimeInputWriter for Runner — the WS attach
// transport's "resize" frame ingress (docs/plans/phase6-container-backend.md
// §PR1's other resize seam is the HTTP route, see Runner.ResizeRuntimeID).
// It resolves jobID to a runtimeID via the jobs table, then adopts a
// SandboxSession and resizes through it. Adopt is bounded — see Subscribe's
// doc comment above for why (session.Resize itself is separately bounded,
// container_backend.go, since its interface signature takes no context to
// inherit one from), including the ctx.Err()-gated Warn + wrapped error
// below, matching WriteInput's just above (next-session-container-backend-
// followups.md #3, Opus review of PR #857).
func (r *Runner) ResizeRuntime(jobID string, size TerminalSize) error {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return fmt.Errorf("runtime not found for job %s", jobID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancel()
	session, adopted := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !adopted {
		if ctx.Err() != nil {
			slog.Warn("resize runtime: adopt did not resolve before the control-call deadline; the terminal was not resized",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
			return fmt.Errorf("resize runtime: engine did not respond in time: %w", ctx.Err())
		}
		return ErrRuntimeUnsupported
	}
	return session.Resize(size)
}

// CloseInput implements RuntimeInputWriter for Runner. It resolves jobID to
// a runtimeID via the jobs table, then adopts a SandboxSession and closes
// its input through it. Adopt is bounded — see Subscribe's doc comment
// above for why, including the ctx.Err()-gated Warn + wrapped error below,
// matching WriteInput/ResizeRuntime above (next-session-container-backend-
// followups.md #3, Opus review of PR #857).
func (r *Runner) CloseInput(jobID string) error {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return fmt.Errorf("runtime not found for job %s", jobID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancel()
	session, adopted := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !adopted {
		if ctx.Err() != nil {
			slog.Warn("close input: adopt did not resolve before the control-call deadline; input was not closed",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
			return fmt.Errorf("close input: engine did not respond in time: %w", ctx.Err())
		}
		return ErrRuntimeUnsupported
	}
	return session.CloseInput()
}
