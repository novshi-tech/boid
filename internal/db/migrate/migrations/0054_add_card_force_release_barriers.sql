-- card_force_release_barriers: one row per card whose execution slot an
-- operator force-released (orchestrator.ForceReleaseCardRequest). While a
-- card's row exists here, automatic (internal-event) card_requests dispatch
-- is suppressed for that card — only a human-issued card command, Go, or an
-- explicit retry clears it (see the callers of ClearCardForceReleaseBarrier).
CREATE TABLE IF NOT EXISTS card_force_release_barriers (
    card_id    TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL
);
