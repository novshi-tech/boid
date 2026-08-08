//go:build linux

package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// RuntimeSubscriber subscribes to live output of a running job identified by
// jobID. finished disambiguates ok=false the same way
// backend.SandboxSession.Subscribe's own finished return does (see its doc
// comment for the full rationale, Opus review of PR #864, B2): true means
// the job is actually done (safe to report as such); false means the job is
// still running but has no live stream to offer right now, which a caller
// MUST NOT collapse into "done" — see ws_attach.go/job_log_sse.go's own
// handling for what each ingress does with the distinction.
type RuntimeSubscriber interface {
	Subscribe(jobID string) (snapshot []byte, ch <-chan []byte, cancel func(), ok bool, finished bool)
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

// jobFinishedBeforeRuntime reports whether jobID's job row is in a
// TERMINAL status (completed or failed) or has no row at all — the cases
// Subscribe's !found branch (below) should report as finished=true. A row
// that exists with status "running" (CreateJob's own default — store.go)
// and no runtime_id yet is NOT finished: the job may still be mid-dispatch
// and about to launch — see Subscribe's own doc comment (Opus review of PR
// #864, NB2) for the window this covers.
//
// Checking status rather than mere row-existence (an earlier version of
// this fix did the latter, via a since-removed jobRowExists) matters for
// jobs that fail BEFORE ever reaching Launch: Runner.failJob (8 call
// sites — spec build / workspace-home resolution / host-command
// resolution, ...) persists a terminal "failed" status row with
// runtime_id left empty forever. The row-existence-only version reported
// finished=false for these — permanently, since no runtime_id will ever
// arrive — which sent `boid job attach`/the Web UI job page into
// reporting "still running, try again" forever for a job that will truly
// never run again (Opus review of PR #864, NB3).
//
// A genuine DB error (anything other than a confirmed absent row)
// deliberately does NOT default to finished=true: unlike "no such row",
// an error tells us nothing about whether the job might still be about to
// launch, so defaulting to finished=true here could reintroduce the exact
// false positive this whole fix removes elsewhere, just from a different
// cause (a flaky DB read instead of a wedged engine).
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
// and "the engine never answered".
//
// Both early ok=false returns below also THINK ABOUT finished rather than
// hardcoding it (Opus review of PR #864, NB2 — a round after the #857
// Warn-logging fix above: the first version of this method logged the
// Warn correctly but still left finished hardcoded at true on both
// branches, which reopened the exact false-positive class B2 had just
// closed for containerSession.Subscribe, just through two different
// doors here):
//
//   - "no runtime_id yet": CreateJob (Dispatch's very first step) persists
//     a job row — and fires r.JobEvents.JobCreated so the Web UI can
//     already show it — well before launchSandbox's own `job.RuntimeID =
//     handleID; UpdateJob(...)` call sets RuntimeID. Everything between
//     those two points (BuildSandboxSpec, ResolveHostCommands, workspace-
//     home resolution/init — PR #861 added a debug log specifically
//     because that step can be slow) can take real wall-clock time, during
//     which a job row exists but has no runtime_id yet. A caller landing
//     here during that window is looking at a job that is ABOUT TO run,
//     not one that is done — jobFinishedBeforeRuntime (below) reports
//     finished=false for it. A job that instead failed BEFORE ever
//     reaching Launch (Runner.failJob, 8 call sites) leaves the same
//     "row exists, no runtime_id" shape behind, but permanently — the
//     status column is what actually distinguishes "still might launch"
//     from "already terminal", not mere row presence (Opus review of PR
//     #864, NB3 — see jobFinishedBeforeRuntime's own doc comment for the
//     concrete failure this closes). A jobID with NO row at all (mistyped,
//     or old enough to have been GC'd) also reports finished=true: nothing
//     will ever come either way.
//   - Adopt itself returning ok=false: distinguishes "Adopt legitimately
//     found nothing" (ctx still had budget left when Adopt returned — the
//     backend has no notion of this container at all, already exited and
//     reaped or never existed — finished=true) from "the
//     sessionControlCallTimeout deadline fired before Adopt could resolve"
//     (ctx.Err() != nil — the same condition the Warn above already logs —
//     a wedged engine told us NOTHING about whether the container is still
//     alive; the job could be very much still running, finished=true here
//     would be exactly B2's false positive reached through the
//     Adopt-itself-timed-out door instead of the adopted-session's-own-
//     Subscribe door). The same ctx.Err() != nil pattern already
//     distinguishes this in StopJobRuntime/SignalJobRuntime/
//     ResizeRuntimeID (this file's own sibling methods) — this is that
//     same pattern applied to the one method that hadn't used it yet.
func (r *Runner) Subscribe(jobID string) (snapshot []byte, ch <-chan []byte, cancel func(), ok bool, finished bool) {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return nil, nil, func() {}, false, r.jobFinishedBeforeRuntime(jobID)
	}
	var result struct {
		snapshot []byte
		ch       <-chan []byte
		cancel   func()
		ok       bool
		finished bool
	}
	adopted, ctxErr, _ := r.withAdoptedSession(context.Background(), runtimeID,
		func(ctx context.Context) {
			slog.Warn("subscribe: adopt did not resolve before the control-call deadline; the runtime's live output cannot be attached to",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
		},
		nil, // unreachable: parentCtx is context.Background(), so ctx.Err() is never Canceled here.
		func(ctx context.Context, session backend.SandboxSession) error {
			result.snapshot, result.ch, result.cancel, result.ok, result.finished = session.Subscribe()
			return nil
		},
	)
	if !adopted {
		return nil, nil, func() {}, false, ctxErr == nil
	}
	return result.snapshot, result.ch, result.cancel, result.ok, result.finished
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
	adopted, ctxErr, fnErr := r.withAdoptedSession(context.Background(), runtimeID,
		func(ctx context.Context) {
			slog.Warn("write input: adopt did not resolve before the control-call deadline; input was not delivered",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
		},
		nil, // unreachable: parentCtx is context.Background(), so ctx.Err() is never Canceled here.
		func(ctx context.Context, session backend.SandboxSession) error {
			return session.WriteInput(data)
		},
	)
	if !adopted {
		if ctxErr != nil {
			return fmt.Errorf("write input: engine did not respond in time: %w", ctxErr)
		}
		return ErrRuntimeUnsupported
	}
	return fnErr
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
	adopted, ctxErr, fnErr := r.withAdoptedSession(context.Background(), runtimeID,
		func(ctx context.Context) {
			slog.Warn("resize runtime: adopt did not resolve before the control-call deadline; the terminal was not resized",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
		},
		nil, // unreachable: parentCtx is context.Background(), so ctx.Err() is never Canceled here.
		func(ctx context.Context, session backend.SandboxSession) error {
			return session.Resize(size)
		},
	)
	if !adopted {
		if ctxErr != nil {
			return fmt.Errorf("resize runtime: engine did not respond in time: %w", ctxErr)
		}
		return ErrRuntimeUnsupported
	}
	return fnErr
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
	adopted, ctxErr, fnErr := r.withAdoptedSession(context.Background(), runtimeID,
		func(ctx context.Context) {
			slog.Warn("close input: adopt did not resolve before the control-call deadline; input was not closed",
				"job_id", jobID, "runtime_id", runtimeID, "timeout", sessionControlCallTimeout.Get())
		},
		nil, // unreachable: parentCtx is context.Background(), so ctx.Err() is never Canceled here.
		func(ctx context.Context, session backend.SandboxSession) error {
			return session.CloseInput()
		},
	)
	if !adopted {
		if ctxErr != nil {
			return fmt.Errorf("close input: engine did not respond in time: %w", ctxErr)
		}
		return ErrRuntimeUnsupported
	}
	return fnErr
}
