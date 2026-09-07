package orchestrator

// Automatic dispatch of queued card_requests rows: claiming a queued row
// atomically re-checks two things a plain ClaimQueuedCardRequests has no way
// to express (it never reads the card's own row) — the card's CURRENT
// status, and whether an operator force-release is still in effect for it.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

var (
	// ErrCardNotEligibleForDispatch: the card is not parked/working at claim
	// time. Every currently-queued request for it is drained (failed) rather
	// than left queued — see ClaimQueuedCardRequestsForDispatch's doc
	// comment for why leaving them queued would loop forever.
	ErrCardNotEligibleForDispatch = errors.New("card request: card is not parked or working; automatic dispatch skipped")
	// ErrCardForceReleaseBarrierActive: an operator force-released this
	// card and no human operation (a command, Go, or an explicit retry) has
	// cleared the barrier yet. The queued request is left untouched.
	ErrCardForceReleaseBarrierActive = errors.New("card request: card was force-released; automatic dispatch is suppressed until a human operation")
	// ErrCardRequestCommandKeyChanged: the caller resolved a command
	// definition for one command_key, but the card's oldest queued request
	// now carries a different one — the caller must re-resolve and retry.
	ErrCardRequestCommandKeyChanged = errors.New("card request: queued head's command_key changed since it was resolved")
)

// IsCardRequestDispatchSkip reports whether err from
// ClaimQueuedCardRequestsForDispatch is a legitimate "nothing claimed this
// time" outcome rather than a genuine failure — every caller (the
// TaskRepository transaction wrapper below, and api.dispatchQueuedCardRequest)
// must treat these identically: a plain "try again later", never a warning-
// worthy error. Kept as one function so the two call sites cannot drift.
func IsCardRequestDispatchSkip(err error) bool {
	return errors.Is(err, ErrNoQueuedCardRequests) ||
		errors.Is(err, ErrCardRequestSlotOccupied) ||
		errors.Is(err, ErrCardNotEligibleForDispatch) ||
		errors.Is(err, ErrCardForceReleaseBarrierActive) ||
		errors.Is(err, ErrCardRequestCommandKeyChanged)
}

// PeekOldestQueuedCardRequest reads cardID's oldest queued request's id and
// command_key WITHOUT claiming it — a caller resolving the command
// definition (which needs project.yaml, unavailable to this package) before
// calling ClaimQueuedCardRequestsForDispatch. The peeked head is re-verified
// fresh inside that call, so a stale peek only ever costs a wasted resolve,
// never a wrong claim. Returns ErrNoQueuedCardRequests when cardID has
// nothing queued.
func PeekOldestQueuedCardRequest(dbtx db.DBTX, cardID string) (id, commandKey string, err error) {
	row := dbtx.QueryRow(
		`SELECT id, command_key FROM card_requests WHERE card_id = ? AND status = ? ORDER BY created_at ASC, id ASC LIMIT 1`,
		cardID, string(CardRequestStatusQueued),
	)
	if serr := row.Scan(&id, &commandKey); serr != nil {
		if errors.Is(serr, sql.ErrNoRows) {
			return "", "", ErrNoQueuedCardRequests
		}
		return "", "", fmt.Errorf("peek oldest queued card request: %w", serr)
	}
	return id, commandKey, nil
}

// failAllQueuedCardRequests fails every currently-queued request for cardID
// in one statement. No queued row is ever folded (folding only happens once
// ClaimQueuedCardRequests promotes a head), so there are no siblings to
// release — a plain bulk UPDATE is the whole operation.
func failAllQueuedCardRequests(dbtx db.DBTX, cardID, reason string) (int64, error) {
	res, err := dbtx.Exec(
		`UPDATE card_requests SET status = ?, error = ?, updated_at = ? WHERE card_id = ? AND status = ?`,
		string(CardRequestStatusFailed), reason, time.Now().UTC(), cardID, string(CardRequestStatusQueued),
	)
	if err != nil {
		return 0, fmt.Errorf("fail all queued card requests: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClaimQueuedCardRequestsForDispatch is ClaimQueuedCardRequests plus the two
// guards automatic (internal-event) dispatch needs that a human command or
// Go already enforce for themselves before ever reaching this package:
//
//  1. The card's CURRENT status must be parked/working. IngestCardEventRequest
//     only checks status at the ACTION's own apply time — a later action
//     within the SAME transaction (e.g. answered{accept} applying a
//     complete/drop verb right after) can leave the card done/dropped by
//     commit time, with a queued row already created against the earlier
//     snapshot. When ineligible, every queued request for the card is
//     drained (failed) instead of launched — leaving them queued would have
//     the periodic recovery sweep re-discover and re-skip the same rows
//     forever.
//  2. The card must not be under an active force-release barrier (an
//     operator's explicit "stop this card" via ForceReleaseCardRequest).
//     Left in place, an orphaned continuation's later write (the KNOWN GAP
//     that force-release does not stop the continuation itself) would
//     otherwise re-queue a fresh request and this claim would relaunch a
//     card the operator just told to stop. The barrier only suppresses
//     automatic dispatch — it never fails the queued row, since it is
//     expected to be cleared shortly (a human command, Go, or an explicit
//     retry all clear it).
//
// expectedCommandKey is the command_key the caller resolved a project.yaml
// definition FOR (via a prior PeekOldestQueuedCardRequest, necessarily
// outside any transaction since resolving needs project.yaml, which this
// package cannot read). This call re-reads the CURRENT head fresh and
// returns ErrCardRequestCommandKeyChanged if it no longer matches, rather
// than launching a request under someone else's command definition.
func ClaimQueuedCardRequestsForDispatch(dbtx db.DBTX, cardID, launcherJobID, expectedCommandKey string, def CardRequestDefinition) (primary *CardRequest, folded []*CardRequest, err error) {
	status, err := GetTaskStatus(dbtx, cardID)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			if _, ferr := failAllQueuedCardRequests(dbtx, cardID, "card no longer exists"); ferr != nil {
				return nil, nil, fmt.Errorf("claim queued card requests for dispatch: %w", ferr)
			}
			return nil, nil, ErrCardNotEligibleForDispatch
		}
		return nil, nil, fmt.Errorf("claim queued card requests for dispatch: get card status: %w", err)
	}
	if status != TaskStatusParked && status != TaskStatusWorking {
		if _, ferr := failAllQueuedCardRequests(dbtx, cardID, fmt.Sprintf("card is %q, not parked or working", status)); ferr != nil {
			return nil, nil, fmt.Errorf("claim queued card requests for dispatch: %w", ferr)
		}
		return nil, nil, ErrCardNotEligibleForDispatch
	}

	blocked, berr := HasCardForceReleaseBarrier(dbtx, cardID)
	if berr != nil {
		return nil, nil, fmt.Errorf("claim queued card requests for dispatch: check force-release barrier: %w", berr)
	}
	if blocked {
		return nil, nil, ErrCardForceReleaseBarrierActive
	}

	headID, headCommandKey, perr := PeekOldestQueuedCardRequest(dbtx, cardID)
	if perr != nil {
		return nil, nil, perr
	}
	if headCommandKey != expectedCommandKey {
		return nil, nil, fmt.Errorf("claim queued card requests for dispatch: head %q command_key is now %q, not the resolved %q: %w",
			headID, headCommandKey, expectedCommandKey, ErrCardRequestCommandKeyChanged)
	}

	return ClaimQueuedCardRequests(dbtx, cardID, launcherJobID, def)
}

// ListCardIDsWithQueuedCardRequests returns every distinct card_id currently
// holding at least one queued card_requests row — the periodic dispatch
// sweep's work list (recovery for a missed immediate dispatch attempt).
func ListCardIDsWithQueuedCardRequests(dbtx db.DBTX) ([]string, error) {
	rows, err := dbtx.Query(`SELECT DISTINCT card_id FROM card_requests WHERE status = ? ORDER BY card_id`, string(CardRequestStatusQueued))
	if err != nil {
		return nil, fmt.Errorf("list card ids with queued card requests: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list card ids with queued card requests: scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list card ids with queued card requests: rows: %w", err)
	}
	return out, nil
}
