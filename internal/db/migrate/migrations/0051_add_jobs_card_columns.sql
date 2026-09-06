-- jobs.card_id / jobs.card_request_id: which card_requests row (if any)
-- this job serves. Unlike JobSpec/the broker's in-memory token registry,
-- these columns survive a daemon restart, which is what lets the
-- card_requests startup recovery scan reverse-lookup a "launching" row's
-- session continuation from the jobs table alone
-- (internal/orchestrator/card_request_release.go's
-- RecoverLaunchingCardRequests).
ALTER TABLE jobs ADD COLUMN card_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN card_request_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_jobs_card_request_id
    ON jobs(card_request_id) WHERE card_request_id != '';
