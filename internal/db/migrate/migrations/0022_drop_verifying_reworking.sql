-- 0022_drop_verifying_reworking.sql
--
-- The state machine is simplified to pending → executing → done (+ aborted).
-- The 'verifying' and 'reworking' statuses are removed. Any in-progress task
-- left in those statuses is force-aborted with an audit-trail action so the
-- timeline still shows where it was when migration ran.
--
-- Legacy verification.findings / lifecycle.rework_count entries in the
-- payload are left untouched; the new state machine simply ignores them.

INSERT INTO actions (id, task_id, type, payload, from_status, to_status, created_at)
SELECT
    lower(hex(randomblob(16))),
    id,
    'abort',
    json_object(
        'code', 'state_machine_migration',
        'message', 'verifying/reworking states removed; task force-aborted by migration 0022'
    ),
    status,
    'aborted',
    CURRENT_TIMESTAMP
FROM tasks
WHERE status IN ('verifying', 'reworking');

UPDATE tasks
SET status = 'aborted',
    updated_at = CURRENT_TIMESTAMP
WHERE status IN ('verifying', 'reworking');
