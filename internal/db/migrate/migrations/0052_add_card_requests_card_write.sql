-- card_requests.launched_card_write: snapshotted alongside the other
-- launched_* columns at queued->launching, independent of any behavior's own
-- readonly. A Go-originated request (command_key = CardRequestCommandKeyGo)
-- keeps the column's default (false).
ALTER TABLE card_requests ADD COLUMN launched_card_write BOOLEAN NOT NULL DEFAULT FALSE;
