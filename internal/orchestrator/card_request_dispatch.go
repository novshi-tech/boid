package orchestrator

// Automatic dispatch of queued card_requests rows: claiming a queued row
// while also re-checking the card's current status and force-release state.

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
// time" outcome rather than a genuine failure.
func IsCardRequestDispatchSkip(err error) bool {
	return errors.Is(err, ErrNoQueuedCardRequests) ||
		errors.Is(err, ErrCardRequestSlotOccupied) ||
		errors.Is(err, ErrCardNotEligibleForDispatch) ||
		errors.Is(err, ErrCardForceReleaseBarrierActive) ||
		errors.Is(err, ErrCardRequestCommandKeyChanged)
}

// PeekOldestQueuedCardRequest reads cardID's oldest queued request's id and
// command_key without claiming it. Returns ErrNoQueuedCardRequests when
// cardID has nothing queued.
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
// in one statement. A queued row is never folded (folding only happens once
// ClaimQueuedCardRequests promotes a head), so there are no siblings to
// release and the bare UPDATE is complete on its own.
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

// ClaimQueuedCardRequestsForDispatch is ClaimQueuedCardRequests plus two
// guards automatic dispatch needs on top of what a human command/Go already
// enforce for themselves: the card must currently be parked/working
// (otherwise every queued request for it is drained instead), and no active
// force-release barrier may be blocking it. Returns
// ErrCardRequestCommandKeyChanged if the queued head's command_key no
// longer matches expectedCommandKey by the time this runs.
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
