package orchestrator

// card_force_release_barriers: one row per card an operator force-released,
// suppressing automatic (internal-event) card_requests dispatch for that
// card until a human operation (a card command, Go, or an explicit retry)
// clears it. See ClaimQueuedCardRequestsForDispatch for the read side.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// SetCardForceReleaseBarrier plants (or refreshes) cardID's barrier.
// Idempotent — setting it again while already set is not an error.
func SetCardForceReleaseBarrier(dbtx db.DBTX, cardID string) error {
	_, err := dbtx.Exec(
		`INSERT INTO card_force_release_barriers (card_id, created_at) VALUES (?, ?)
		 ON CONFLICT(card_id) DO UPDATE SET created_at = excluded.created_at`,
		cardID, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("set card force release barrier: %w", err)
	}
	return nil
}

// ClearCardForceReleaseBarrier removes cardID's barrier, if any. Idempotent
// — clearing an already-clear barrier is not an error.
func ClearCardForceReleaseBarrier(dbtx db.DBTX, cardID string) error {
	if _, err := dbtx.Exec(`DELETE FROM card_force_release_barriers WHERE card_id = ?`, cardID); err != nil {
		return fmt.Errorf("clear card force release barrier: %w", err)
	}
	return nil
}

// HasCardForceReleaseBarrier reports whether cardID currently has an active
// force-release barrier.
func HasCardForceReleaseBarrier(dbtx db.DBTX, cardID string) (bool, error) {
	row := dbtx.QueryRow(`SELECT 1 FROM card_force_release_barriers WHERE card_id = ?`, cardID)
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("has card force release barrier: %w", err)
	}
	return true, nil
}
