//go:build linux

package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// RuntimeSubscriber subscribes to live output of a running job identified by
// jobID. finished disambiguates ok=false: true means the job is actually
// done (safe to report as such); false means it is still running but has no
// live stream to offer right now, which a caller must not collapse into
// "done" — see ws_attach.go/job_log_sse.go for how each ingress handles it.
type RuntimeSubscriber interface {
	Subscribe(jobID string) (snapshot RuntimeSnapshot, ch <-chan []byte, cancel func(), ok bool, finished bool)
}

// RuntimeInputWriter provides write access to a running job's PTY input.
type RuntimeInputWriter interface {
	WriteInput(jobID string, data []byte) error
	ResizeRuntime(jobID string, size TerminalSize) error
	// CloseInput signals that no more input is coming for jobID. A no-op for
	// jobs whose runtime has no notion of "closing" input (interactive PTY
	// sessions, or non-interactive sessions with no StdinForward pipe).
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

// jobFinishedBeforeRuntime reports whether jobID's job row is in a terminal
// status (completed or failed) or has no row at all — the cases Subscribe's
// !found branch (below) should report as finished=true. A row with status
// "running" and no runtime_id yet is NOT finished: the job may still be
// mid-dispatch and about to launch.
//
// This checks status rather than mere row existence: Runner.failJob can
// persist a terminal "failed" row with runtime_id left empty forever, so
// row-existence alone would report finished=false for it permanently.
//
// A genuine DB error deliberately does not default to finished=true: unlike
// "no such row", an error tells us nothing about whether the job might
// still be about to launch.
func (r *Runner) jobFinishedBeforeRuntime(jobID string) bool {
	var status string
	err := r.DB.QueryRow(`SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return true
	case err != nil:
		return false
	default:
		return JobStatus(status) == JobStatusCompleted || JobStatus(status) == JobStatusFailed
	}
}

// Subscribe implements RuntimeSubscriber for Runner. It resolves jobID to a
// runtimeID via the jobs table, then adopts a SandboxSession for it — the
// stream-adoption seam shared by WS attach and the Web UI's SSE follow
// endpoint, both of which call through this same method.
//
// Adopt runs under sessionControlCallTimeout, not context.Background(): a
// cache-miss Adopt (fresh daemon restart, before containerBackend's session
// cache is repopulated) does a real `docker inspect` against the engine, and
// an unbounded context here would hang a WS attach or SSE follow request
// forever against a wedged engine.
//
// When Adopt's !adopted is caused by that deadline firing (ctx.Err() != nil)
// rather than a legitimate "no such runtime" result, this logs a Warn naming
// the cause — otherwise a wedged engine and an already-exited job are
// indistinguishable to an operator reading the logs, both surfacing as
// silent ok=false.
//
// Both early ok=false returns below think about finished rather than
// hardcoding it:
//
//   - "no runtime_id yet": a job row can exist with no runtime_id for real
//     wall-clock time while dispatch is still in progress (BuildSandboxSpec,
//     workspace-home resolution, ...) — that job is about to run, not done.
//     jobFinishedBeforeRuntime (above) is what actually distinguishes "still
//     might launch" from "already terminal" for it, not mere row presence.
//     A jobID with no row at all also reports finished=true.
//   - Adopt itself returning ok=false: distinguishes "Adopt legitimately
//     found nothing" (ctx still had budget left — the backend has no notion
//     of this container at all — finished=true) from "the
//     sessionControlCallTimeout deadline fired before Adopt could resolve"
//     (ctx.Err() != nil — a wedged engine told us nothing about whether the
//     container is still alive, so finished=true here would be a false
//     positive). The same ctx.Err() != nil pattern is used by
//     StopJobRuntime/SignalJobRuntime/ResizeRuntimeID.
func (r *Runner) Subscribe(jobID string) (snapshot RuntimeSnapshot, ch <-chan []byte, cancel func(), ok bool, finished bool) {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return RuntimeSnapshot{}, nil, func() {}, false, r.jobFinishedBeforeRuntime(jobID)
	}
	ctx, cancelCtx := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancelCtx()
	session, adopted := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !adopted {
		if ctx.Err() != nil {
			slog.Warn("subscribe: adopt did not resolve before the control-call deadline; the runtime's live output cannot be attached to",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
		}
		return RuntimeSnapshot{}, nil, func() {}, false, ctx.Err() == nil
	}
	return session.Subscribe()
}

// WriteInput implements RuntimeInputWriter for Runner. It resolves jobID to
// a runtimeID via the jobs table, then adopts a SandboxSession and writes
// through it. Adopt is bounded — see Subscribe's doc comment above for why.
//
// Unlike Subscribe, WriteInput has an error return, so an Adopt-deadline
// !adopted also gets a wrapped error distinguishing it from the ordinary
// ErrRuntimeUnsupported result — the same "engine did not respond in time"
// phrasing ResizeRuntimeID (runner.go) uses for session.Resize's own
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
// transport's "resize" frame ingress (the other resize seam is the HTTP
// route, see Runner.ResizeRuntimeID). It resolves jobID to a runtimeID via
// the jobs table, then adopts a SandboxSession and resizes through it.
// Adopt is bounded — see Subscribe's doc comment above for why
// (session.Resize itself is separately bounded in container_backend.go,
// since its interface signature takes no context to inherit one from).
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
// above for why.
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
