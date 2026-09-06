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

	"github.com/novshi-tech/boid/internal/db"
)

// jobs.status の終端値。dispatcher.JobStatusCompleted/JobStatusFailed の文字列と
// 一致させること (dispatcher はこのパッケージを import しているので逆方向の
// import はできない — GCTasks の raw SQL と同じ制約)。
const (
	jobStatusCompletedLiteral = "completed"
	jobStatusFailedLiteral    = "failed"
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
//     aborted/dropped -> failed).
//   - target_kind=session: the continuation JOB's terminal status
//     (completed -> finished, failed -> failed). A Run's own hook job
//     finishing does not count — only the session job named by target_id.
//
// A continuation that is still live, or whose target row cannot be read
// yet, is left untouched: this function never guesses. Call it repeatedly
// (a periodic loop) rather than relying on a single pass.
//
// Each row's read-outcome-then-write runs in its OWN transaction (conn is
// *sql.DB, not db.DBTX, specifically so this can call db.InTxDB per row) —
// FinishCardRequest/FailCardRequest are themselves multiple statements
// (closing folded siblings, absorbing older failures) that must not land
// half-applied, but one row's failure must not roll back every other row
// this pass already released.
func ReconcileCardRequestSlots(conn *sql.DB) ([]CardRequestSlotOutcome, error) {
	rows, err := conn.Query(cardRequestSelectCols+` FROM card_requests WHERE status = ?`, string(CardRequestStatusAttached))
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
			return outcomes, fmt.Errorf("reconcile card request %q: %w", req.ID, err)
		}
		if outcome != nil {
			outcomes = append(outcomes, *outcome)
		}
	}
	return outcomes, nil
}

// continuationTerminalOutcome reports whether req's continuation has reached
// a terminal state and, if so, whether that terminal state counts as
// success. ok is false when the continuation is still live or its target
// row could not be read (deleted, or a target_id that never resolved).
func continuationTerminalOutcome(dbtx db.DBTX, req *CardRequest) (finished, ok bool, err error) {
	switch req.TargetKind {
	case CardRequestTargetKindTask:
		task, err := GetTask(dbtx, req.TargetID)
		if err != nil {
			if errors.Is(err, ErrTaskNotFound) {
				return false, false, nil
			}
			return false, false, fmt.Errorf("get task %q: %w", req.TargetID, err)
		}
		if !IsTerminalStatus(task.Status) {
			return false, false, nil
		}
		return task.Status == TaskStatusDone, true, nil
	case CardRequestTargetKindSession:
		row := dbtx.QueryRow(`SELECT status FROM jobs WHERE id = ?`, req.TargetID)
		var status string
		if err := row.Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, false, nil
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
		return false, false, nil
	}
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
// in-memory state) for a job that isn't the launcher itself. Found -> attach
// it. Not found -> the launcher exited before creating anything, so the
// request goes to failed (retry-able), never finished — releasing a slot on
// failure is not the same as a successful judgment.
//
// One transaction per row — same reasoning as ReconcileCardRequestSlots.
func RecoverLaunchingCardRequests(conn *sql.DB) ([]CardRequestSlotOutcome, error) {
	rows, err := conn.Query(cardRequestSelectCols+` FROM card_requests WHERE status = ?`, string(CardRequestStatusLaunching))
	if err != nil {
		return nil, fmt.Errorf("recover launching card requests: list: %w", err)
	}
	launching, err := scanCardRequests(rows)
	if err != nil {
		return nil, fmt.Errorf("recover launching card requests: %w", err)
	}

	var outcomes []CardRequestSlotOutcome
	for _, req := range launching {
		var outcome CardRequestSlotOutcome
		err := db.InTxDB(conn, func(tx db.DBTX) error {
			jobRow := tx.QueryRow(
				`SELECT id FROM jobs WHERE card_request_id = ? AND id != ? ORDER BY created_at ASC LIMIT 1`,
				req.ID, req.LauncherJobID,
			)
			var jobID string
			switch err := jobRow.Scan(&jobID); {
			case errors.Is(err, sql.ErrNoRows):
				if ferr := FailCardRequest(tx, req.ID, "launcher exited before creating a continuation (daemon restart recovery)"); ferr != nil {
					return fmt.Errorf("fail: %w", ferr)
				}
				outcome = CardRequestSlotOutcome{RequestID: req.ID, Status: string(CardRequestStatusFailed)}
				return nil
			case err != nil:
				return fmt.Errorf("find continuation job: %w", err)
			default:
				if aerr := AttachCardRequest(tx, req.ID, CardRequestTargetKindSession, jobID); aerr != nil {
					return fmt.Errorf("attach %q: %w", jobID, aerr)
				}
				outcome = CardRequestSlotOutcome{RequestID: req.ID, Status: string(CardRequestStatusAttached)}
				return nil
			}
		})
		if err != nil {
			return outcomes, fmt.Errorf("recover card request %q: %w", req.ID, err)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// ForceReleaseCardRequest is the operator escape hatch for a stuck slot:
// fails a queued/launching/attached row regardless of whether its
// continuation has actually terminated. Thin wrapper over FailCardRequest —
// forcing is exactly "this slot is stuck, close it out and let a human or a
// retry deal with the fallout", which is what a failed (retry-able) request
// already means.
func ForceReleaseCardRequest(dbtx db.DBTX, id, reason string) error {
	if reason == "" {
		reason = "force-released by operator"
	}
	return FailCardRequest(dbtx, id, reason)
}
