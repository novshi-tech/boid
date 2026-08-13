-- Phase 1 PR-5a (docs/plans/cross-project-issue-triage.md 決定14): make
-- "has a task_triage row" a reliable discriminator for "is a triage task".
--
-- api.TaskAppService.CreateTask now seeds an empty sidecar row for every task
-- created into a pre-execution status, so the invariant holds from birth
-- going forward. Rows created BEFORE that (PR-1〜4 era) only got a sidecar
-- once their first side-effect action (park / attrs_set / child_added /
-- child_specced) landed, so a triage task that never received one is
-- invisible to the predicate. Two things rest on it:
--
--   * api.TaskWorkflowService.ListTriage's listing (a card with no row is
--     silently absent from the queue read surface), and
--   * PR-5b's reopen routing (a card with no row would take reopen's
--     ORDINARY rule, done/aborted → executing, i.e. try to RUN the card).
--
-- Backfilling the rows retroactively closes both. The insert is empty-valued
-- (kind/urgency/detail take their column defaults), which is exactly what
-- CreateTask seeds — it asserts "this is a triage task" and nothing else, so
-- it cannot invent state that was never observed.
--
-- Scope: only the statuses that are triage-exclusive. `done`/`aborted` are
-- shared with ordinary tasks and are deliberately NOT backfilled — there is
-- no way to tell a legacy done triage task from a done executor task, and
-- mislabeling an executor task would be the more damaging error. This costs
-- nothing in practice: working→done did not exist before PR-5b, so no triage
-- task can have reached done already.
INSERT INTO task_triage (task_id)
SELECT t.id FROM tasks t
WHERE t.status IN ('captured', 'triaged', 'parked', 'ready', 'working', 'dropped')
  AND NOT EXISTS (SELECT 1 FROM task_triage tt WHERE tt.task_id = t.id);

-- Second half (Opus review, Medium): promote any urgency/kind that a
-- pre-PR-5a attrs_set folded into the opaque blob up into the real columns,
-- then remove them from the blob.
--
-- PR-5a made attrs_set write these two keys to the columns and deliberately
-- NOT to detail.attrs, so the value the queue SQL reads and the value khi
-- reads back can never disagree. That guarantee only covers writes made after
-- the change: a card whose blob already carries detail.attrs.urgency would
-- keep serving the stale blob copy forever while the column moved on — the
-- exact drift the no-duplication rule exists to prevent. Converging the
-- existing rows here makes the invariant hold for the whole table rather than
-- only for rows touched since.
--
-- The promotion is guarded so it can never lose information: it only fires
-- when the column is empty and the blob holds a value from the closed
-- vocabulary the daemon accepts (a blob value outside it was never usable as
-- a queue predicate anyway, and is dropped with the key rather than promoted
-- into a column the queue SQL would ignore).
UPDATE task_triage
SET urgency = json_extract(detail, '$.attrs.urgency')
WHERE urgency = ''
  AND json_valid(detail)
  AND json_extract(detail, '$.attrs.urgency') IN ('now', 'today', 'week', 'someday');

UPDATE task_triage
SET kind = json_extract(detail, '$.attrs.kind')
WHERE kind = ''
  AND json_valid(detail)
  AND json_extract(detail, '$.attrs.kind') IN ('signal', 'issue', 'theme');

UPDATE task_triage
SET detail = json_remove(detail, '$.attrs.urgency', '$.attrs.kind')
WHERE json_valid(detail)
  AND (json_extract(detail, '$.attrs.urgency') IS NOT NULL
       OR json_extract(detail, '$.attrs.kind') IS NOT NULL);
