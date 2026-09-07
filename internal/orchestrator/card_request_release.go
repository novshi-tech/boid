package orchestrator

// card_requests の実行枠を、継続先の終端を照合して解放する処理。
// 時間経過だけで解放する trigger の self-heal (TriggerRunSelfHealGrace) と
// は別物 — 継続先の生存が確認できない限り枠は空けず、次の呼び出しで
// 照合を再試行する。dispatcher パッケージを import できない (循環) ため、
// GCTasks と同じ流儀で jobs テーブルへ直接 raw SQL を投げる。

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// jobs.status/role の値。dispatcher.JobStatusCompleted/JobStatusFailed および
// JobKindSession の文字列と一致させること (dispatcher はこのパッケージを
// import しているので逆方向の import はできない — GCTasks の raw SQL と
// 同じ制約)。
const (
	jobStatusCompletedLiteral = "completed"
	jobStatusFailedLiteral    = "failed"
	jobRoleSessionLiteral     = "session"
)

// CardRequestSlotOutcome is one row ReconcileCardRequestSlots or
// RecoverLaunchingCardRequests actually changed.
type CardRequestSlotOutcome struct {
	RequestID string
	// Status is the CardRequest status the row moved to: "finished",
	// "failed", or "attached" (RecoverLaunchingCardRequests reattaching a
	// found continuation job).
	Status string
}

// ReconcileCardRequestSlots scans every attached card_requests row and
// releases the ones whose continuation has reached a terminal state:
//   - target_kind=task: the task's own terminal status (done -> finished,
//     aborted/dropped -> failed), or the task row no longer existing at all
//     (deleted -> failed, same as an aborted continuation).
//   - target_kind=session: the continuation JOB's terminal status
//     (completed -> finished, failed -> failed). A Run's own hook job
//     finishing does not count — only the session job named by target_id.
//
// A continuation that is still live is left untouched: this function never
// releases on a guess. Call it repeatedly (a periodic loop) rather than
// relying on a single pass.
//
// Each row's read-outcome-then-write runs in its OWN transaction (conn is
// *sql.DB, not db.DBTX, specifically so this can call db.InTxDB per row) —
// FinishCardRequest/FailCardRequest are themselves multiple statements
// (closing folded siblings, absorbing older failures) that must not land
// half-applied, but one row's failure must not roll back every other row
// this pass already released. A single row's error (e.g. it moved out of
// "attached" between the listing query above and this row's own
// transaction, because a concurrent force-release beat this pass to it) is
// logged and skipped rather than aborting the whole pass — a listing query
// with no snapshot isolation across many rows can't assume none of them
// raced.
func ReconcileCardRequestSlots(conn *sql.DB) ([]CardRequestSlotOutcome, error) {
	rows, err := conn.Query(cardRequestSelectCols+` FROM card_requests WHERE status = ? ORDER BY created_at ASC`, string(CardRequestStatusAttached))
	if err != nil {
		return nil, fmt.Errorf("reconcile card request slots: list attached: %w", err)
	}
	attached, err := scanCardRequests(rows)
	if err != nil {
		return nil, fmt.Errorf("reconcile card request slots: %w", err)
	}

	var outcomes []CardRequestSlotOutcome
	for _, req := range attached {
		var outcome *CardRequestSlotOutcome
		err := db.InTxDB(conn, func(tx db.DBTX) error {
			finished, terminal, err := continuationTerminalOutcome(tx, req)
			if err != nil {
				return err
			}
			if !terminal {
				return nil
			}
			if finished {
				if err := FinishCardRequest(tx, req.ID, "continuation reached a terminal successful state"); err != nil {
					return fmt.Errorf("finish: %w", err)
				}
				outcome = &CardRequestSlotOutcome{RequestID: req.ID, Status: string(CardRequestStatusFinished)}
				return nil
			}
			if err := FailCardRequest(tx, req.ID, "continuation ended without success"); err != nil {
				return fmt.Errorf("fail: %w", err)
			}
			outcome = &CardRequestSlotOutcome{RequestID: req.ID, Status: string(CardRequestStatusFailed)}
			return nil
		})
		if err != nil {
			slog.Warn("reconcile card request slot: skipping this row, pass continues", "request_id", req.ID, "error", err)
			continue
		}
		if outcome != nil {
			outcomes = append(outcomes, *outcome)
		}
	}
	return outcomes, nil
}

// continuationTerminalOutcome reports whether req's continuation has reached
// a terminal state and, if so, whether that terminal state counts as
// success. ok is false only while the continuation is still live. A target
// row that no longer exists at all (deleted out from under an attached
// request) is treated as a non-success terminal state, not "still live" —
// a deleted task/job can never report back, so leaving the slot attached
// forever would be the exact stuck-slot failure this function exists to
// prevent.
func continuationTerminalOutcome(dbtx db.DBTX, req *CardRequest) (finished, ok bool, err error) {
	switch req.TargetKind {
	case CardRequestTargetKindTask:
		status, err := GetTaskStatus(dbtx, req.TargetID)
		if err != nil {
			if errors.Is(err, ErrTaskNotFound) {
				return false, true, nil
			}
			return false, false, fmt.Errorf("get task status %q: %w", req.TargetID, err)
		}
		if !IsTerminalStatus(status) {
			return false, false, nil
		}
		return status == TaskStatusDone, true, nil
	case CardRequestTargetKindSession:
		row := dbtx.QueryRow(`SELECT status FROM jobs WHERE id = ?`, req.TargetID)
		var status string
		if err := row.Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, true, nil
			}
			return false, false, fmt.Errorf("get job %q: %w", req.TargetID, err)
		}
		switch status {
		case jobStatusCompletedLiteral:
			return true, true, nil
		case jobStatusFailedLiteral:
			return false, true, nil
		default:
			return false, false, nil
		}
	default:
		// Should never happen (AttachCardRequest only accepts task/session).
		// Warn rather than silently treat it as stuck-forever-live.
		slog.Warn("continuation terminal outcome: unrecognized target_kind, treating as still running (will retry every pass, never resolves on its own)",
			"request_id", req.ID, "target_kind", req.TargetKind, "target_id", req.TargetID)
		return false, false, nil
	}
}

// launcherJobRowCreationGrace bounds how long a launching row may go
// without a matching jobs row before ReconcileLaunchingCardRequests treats
// it as gone. The card_requests INSERT and the jobs INSERT are not the same
// transaction (StartExec's Dispatch() writes the jobs row only after
// resolving the project, building the JobSpec, and a git subprocess call),
// so a reconcile tick can legitimately land in that gap. Far larger than
// that gap ever takes, so it never delays detecting a genuinely gone
// launcher by more than this.
const launcherJobRowCreationGrace = 60 * time.Second

// launcherJobTerminalOrGone reports whether the launcher job identified by
// launcherJobID has reached a terminal status (completed/failed). A missing
// job row is terminal only once updatedAt (the row's launching promotion
// time) is older than launcherJobRowCreationGrace — see that constant's own
// doc comment for why a missing row isn't immediately treated as gone.
func launcherJobTerminalOrGone(dbtx db.DBTX, launcherJobID string, updatedAt time.Time) (bool, error) {
	row := dbtx.QueryRow(`SELECT status FROM jobs WHERE id = ?`, launcherJobID)
	var status string
	if err := row.Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Since(updatedAt) >= launcherJobRowCreationGrace, nil
		}
		return false, fmt.Errorf("get launcher job %q: %w", launcherJobID, err)
	}
	switch status {
	case jobStatusCompletedLiteral, jobStatusFailedLiteral:
		return true, nil
	default:
		return false, nil
	}
}

// attachFoundContinuationOrFail reverse-looks-up a SESSION job carrying
// req's card_request_id (created at or after req.UpdatedAt, i.e. this
// row's current launching promotion — see RecoverLaunchingCardRequests' own
// doc comment for why the time bound matters for a retried request) and
// attaches it if found, or fails req (retry-able) if not. Shared body for
// RecoverLaunchingCardRequests (daemon-startup, unconditional) and
// ReconcileLaunchingCardRequests (periodic, gated on the launcher job
// itself having terminated).
func attachFoundContinuationOrFail(tx db.DBTX, req *CardRequest, failReason string) (*CardRequestSlotOutcome, error) {
	jobRow := tx.QueryRow(
		`SELECT id FROM jobs WHERE card_request_id = ? AND card_request_id != '' AND id != ? AND role = ? AND created_at >= ?
		 ORDER BY created_at ASC LIMIT 1`,
		req.ID, req.LauncherJobID, jobRoleSessionLiteral, req.UpdatedAt,
	)
	var jobID string
	switch err := jobRow.Scan(&jobID); {
	case errors.Is(err, sql.ErrNoRows):
		if ferr := FailCardRequest(tx, req.ID, failReason); ferr != nil {
			return nil, fmt.Errorf("fail: %w", ferr)
		}
		return &CardRequestSlotOutcome{RequestID: req.ID, Status: string(CardRequestStatusFailed)}, nil
	case err != nil:
		return nil, fmt.Errorf("find continuation job: %w", err)
	default:
		if aerr := AttachCardRequest(tx, req.ID, CardRequestTargetKindSession, jobID); aerr != nil {
			return nil, fmt.Errorf("attach %q: %w", jobID, aerr)
		}
		return &CardRequestSlotOutcome{RequestID: req.ID, Status: string(CardRequestStatusAttached)}, nil
	}
}

// ReconcileLaunchingCardRequests is the PERIODIC counterpart to
// RecoverLaunchingCardRequests: it clears a card_requests row stuck
// "launching" because its `run:` script exited (or was killed) without
// ever calling `boid task create` / `boid agent start`, without waiting for
// a daemon restart.
//
// Gated on the LAUNCHER JOB's own terminal status (launcherJobTerminalOrGone),
// never on elapsed time: a time-based release could fire while the launcher
// is still legitimately running a long `run:` script, letting a second
// claim through alongside it.
//
// CardRequestCommandKeyGo rows are skipped: a Go reservation (acceptGo,
// workflow_card.go) has no real launcher job — task creation happens
// synchronously in-process — so treating a missing job row as "terminal"
// would fail a Go reservation still legitimately mid-flight between its own
// CreateCardRequest and CreateTaskLinkedToCardRequest calls. acceptGo
// releases its own reservation synchronously on every error path instead;
// only a full daemon crash can leave one stuck, and
// RecoverLaunchingCardRequests' startup scan (unconditional) handles that.
func ReconcileLaunchingCardRequests(conn *sql.DB) ([]CardRequestSlotOutcome, error) {
	rows, err := conn.Query(cardRequestSelectCols+` FROM card_requests WHERE status = ? ORDER BY created_at ASC`, string(CardRequestStatusLaunching))
	if err != nil {
		return nil, fmt.Errorf("reconcile launching card requests: list: %w", err)
	}
	launching, err := scanCardRequests(rows)
	if err != nil {
		return nil, fmt.Errorf("reconcile launching card requests: %w", err)
	}

	var outcomes []CardRequestSlotOutcome
	for _, req := range launching {
		if req.CommandKey == CardRequestCommandKeyGo {
			continue
		}
		var outcome *CardRequestSlotOutcome
		err := db.InTxDB(conn, func(tx db.DBTX) error {
			terminal, terr := launcherJobTerminalOrGone(tx, req.LauncherJobID, req.UpdatedAt)
			if terr != nil {
				return terr
			}
			if !terminal {
				// The launcher is still (or may still be) running normally —
				// leave the row alone. Call this repeatedly (a periodic loop),
				// not on a single pass.
				return nil
			}
			found, aerr := attachFoundContinuationOrFail(tx, req, "launcher exited without creating a continuation")
			if aerr != nil {
				return aerr
			}
			outcome = found
			return nil
		})
		if err != nil {
			slog.Warn("reconcile launching card request: skipping this row, pass continues", "request_id", req.ID, "error", err)
			continue
		}
		if outcome != nil {
			outcomes = append(outcomes, *outcome)
		}
	}
	return outcomes, nil
}

// RecoverLaunchingCardRequests is the daemon-startup recovery scan:
// task continuations write request->task in the SAME transaction as
// CreateTask, so they cannot land here mid-flight. A session continuation
// cannot — Dispatch (an external process launch) can't share a transaction
// with the DB write that records it — so a request can restart the daemon
// still "launching" with the session job already created but never attached.
//
// For every such row, this reverse-looks-up jobs.card_request_id (persisted
// on the session job at Dispatch time, independent of the crashed daemon's
// in-memory state) for the SESSION job (role='session', never the launcher's
// own exec/hook job) created at or after this row's current launching
// promotion (req.UpdatedAt — stamped atomically with launcher_job_id by
// ClaimQueuedCardRequests/the launching fast path). The time bound matters:
// a retried request keeps its id, so a PRIOR attempt's session job can still
// carry the same card_request_id, and without it the newest attempt could
// reattach to a stale, already-finished session from an earlier attempt.
// Found -> attach it. Not found -> the launcher exited before creating
// anything, so the request goes to failed (retry-able), never finished —
// releasing a slot on failure is not the same as a successful judgment.
//
// One transaction per row, and a single row's error is logged and skipped
// rather than aborting the whole scan — same reasoning as
// ReconcileCardRequestSlots, but more important here: this scan runs once at
// startup, not on a ticker, so one bad row aborting the loop would leave
// every OTHER launching row unrecovered until the next daemon restart.
func RecoverLaunchingCardRequests(conn *sql.DB) ([]CardRequestSlotOutcome, error) {
	rows, err := conn.Query(cardRequestSelectCols+` FROM card_requests WHERE status = ? ORDER BY created_at ASC`, string(CardRequestStatusLaunching))
	if err != nil {
		return nil, fmt.Errorf("recover launching card requests: list: %w", err)
	}
	launching, err := scanCardRequests(rows)
	if err != nil {
		return nil, fmt.Errorf("recover launching card requests: %w", err)
	}

	var outcomes []CardRequestSlotOutcome
	for _, req := range launching {
		var outcome *CardRequestSlotOutcome
		err := db.InTxDB(conn, func(tx db.DBTX) error {
			found, aerr := attachFoundContinuationOrFail(tx, req, "launcher exited before creating a continuation (daemon restart recovery)")
			if aerr != nil {
				return aerr
			}
			outcome = found
			return nil
		})
		if err != nil {
			slog.Warn("recover launching card request: skipping this row, scan continues", "request_id", req.ID, "error", err)
			continue
		}
		if outcome != nil {
			outcomes = append(outcomes, *outcome)
		}
	}
	return outcomes, nil
}

// ForceReleaseCardRequest is the operator escape hatch for a stuck slot:
// fails a queued/launching/attached row regardless of whether its
// continuation has actually terminated. Unlike FailCardRequest, it does NOT
// requeue folded siblings — it fails them too, since requeuing would let
// the very next claim restart the card the operator just told to stop.
// Returns every sibling it force-failed this way, since fold is scoped to
// the card rather than id's own command_key/cause_id.
func ForceReleaseCardRequest(dbtx db.DBTX, id, reason string) ([]ForceReleasedSibling, error) {
	if reason == "" {
		reason = "force-released by operator"
	}
	return failCardRequest(dbtx, id, reason, foldedSiblingsFail)
}

// ReleaseCardRequestForTerminalTarget releases the attached card_requests
// row (if any) targeting (targetKind, targetID) the instant that target
// reaches a terminal state, instead of waiting for
// ReconcileCardRequestSlots' next tick — otherwise every occupancy check
// (cardSlotOccupied, cardSlotConflictWithRequests) keeps reporting the slot
// occupied for up to the reconcile interval after the real work already
// finished. found is false in the common case (most terminal tasks/jobs are
// not a card_requests continuation at all).
func ReleaseCardRequestForTerminalTarget(dbtx db.DBTX, targetKind, targetID string, success bool) (found bool, err error) {
	row := dbtx.QueryRow(
		`SELECT id FROM card_requests WHERE target_kind = ? AND target_id = ? AND status = ?`,
		targetKind, targetID, string(CardRequestStatusAttached),
	)
	var id string
	if serr := row.Scan(&id); serr != nil {
		if errors.Is(serr, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("find attached card request for %s %q: %w", targetKind, targetID, serr)
	}
	if success {
		if ferr := FinishCardRequest(dbtx, id, "continuation reached a terminal successful state"); ferr != nil {
			return false, fmt.Errorf("finish: %w", ferr)
		}
	} else if ferr := FailCardRequest(dbtx, id, "continuation ended without success"); ferr != nil {
		return false, fmt.Errorf("fail: %w", ferr)
	}
	return true, nil
}
