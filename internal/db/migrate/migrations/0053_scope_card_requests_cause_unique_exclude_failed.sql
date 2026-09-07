-- idx_card_requests_cause_unique had no status predicate, so a cause_id
-- whose request ended in failed permanently blocked that cause_id from ever
-- being redelivered. Excluding failed rows keeps the dedup for finished
-- (already-handled) causes while letting a retry-able failure's cause_id
-- be redelivered.
DROP INDEX IF EXISTS idx_card_requests_cause_unique;
CREATE UNIQUE INDEX IF NOT EXISTS idx_card_requests_cause_unique_non_failed
    ON card_requests(cause_id) WHERE cause_id != '' AND status != 'failed';
