//go:build linux

package dispatcher

import (
	"context"
	"fmt"
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

// jobRowExists reports whether a jobs row for jobID exists at all,
// independently of whether it has a runtime_id yet. Subscribe's own
// !found branch uses this to tell "no row" (jobID typo'd, or a job old
// enough to have been GC'd — nothing will ever come, finished=true is
// correct) apart from "row exists, no runtime_id yet" (the job is
// mid-dispatch and may still start running, finished=false) — see
// Subscribe's own doc comment (Opus review of PR #864, NB2) for the full
// rationale. A second, separate query rather than widening
// runtimeIDForJob's own return signature: that function has three other
// callers (WriteInput/ResizeRuntime/CloseInput) that only ever need
// found, and this distinction matters to exactly one of Subscribe's two
// early-return branches.
func (r *Runner) jobRowExists(jobID string) bool {
	var one int
	return r.DB.QueryRow(`SELECT 1 FROM jobs WHERE id = ?`, jobID).Scan(&one) == nil
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
// Both early ok=false returns below THINK ABOUT finished rather than
// hardcoding it (Opus review of PR #864, NB2 — a second round after the
// B2 fix above: the first version of this method left both hardcoded at
// finished=true, which turned out to reopen the exact false-positive class
// B2 had just closed, just through two different doors):
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
//     not one that is done. jobRowExists (below) distinguishes that case
//     (finished=false: something may still happen) from a jobID with NO
//     row at all — mistyped, or old enough to have been GC'd — which will
//     never resolve (finished=true: a clean, terminating answer is
//     correct, and the one this branch always gave before this field
//     existed).
//   - Adopt itself returning ok=false: distinguishes "Adopt legitimately
//     found nothing" (ctx still had budget left when Adopt returned — the
//     backend has no notion of this container at all, already exited and
//     reaped or never existed — finished=true) from "the
//     sessionControlCallTimeout deadline fired before Adopt could resolve"
//     (ctx.Err() != nil — a wedged engine told us NOTHING about whether the
//     container is still alive; the job could be very much still running,
//     finished=true here would be exactly B2's false positive reached
//     through the Adopt-itself-timed-out door instead of the adopted-
//     session's-own-Subscribe door). The same ctx.Err() != nil pattern
//     already distinguishes this in StopJobRuntime/SignalJobRuntime/
//     ResizeRuntimeID (this file's own sibling methods) — this is that
//     same pattern applied to the one method that hadn't used it yet.
func (r *Runner) Subscribe(jobID string) (snapshot []byte, ch <-chan []byte, cancel func(), ok bool, finished bool) {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return nil, nil, func() {}, false, !r.jobRowExists(jobID)
	}
	ctx, cancelCtx := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancelCtx()
	session, adopted := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !adopted {
		return nil, nil, func() {}, false, ctx.Err() == nil
	}
	return session.Subscribe()
}

// WriteInput implements RuntimeInputWriter for Runner. It resolves jobID to
// a runtimeID via the jobs table, then adopts a SandboxSession and writes
// through it. Adopt is bounded — see Subscribe's doc comment just above for
// why.
func (r *Runner) WriteInput(jobID string, data []byte) error {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return fmt.Errorf("runtime not found for job %s", jobID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancel()
	session, adopted := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !adopted {
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
// inherit one from).
func (r *Runner) ResizeRuntime(jobID string, size TerminalSize) error {
	runtimeID, found := r.runtimeIDForJob(jobID)
	if !found {
		return fmt.Errorf("runtime not found for job %s", jobID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionControlCallTimeout.Get())
	defer cancel()
	session, adopted := r.sandboxBackend().Adopt(ctx, runtimeID)
	if !adopted {
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
		return ErrRuntimeUnsupported
	}
	return session.CloseInput()
}
