-- card_requests.instruction: the user's free-text instruction for this
-- request, kept as durable row state (not a launch-time argument) so an
-- occupied-slot request's input survives until it is actually claimed and
-- launched (internal/orchestrator/card_request.go).
ALTER TABLE card_requests ADD COLUMN instruction TEXT NOT NULL DEFAULT '';
