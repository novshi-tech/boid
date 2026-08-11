-- Phase 1 PR-4 (docs/plans/cross-project-issue-triage.md 論点7, codex review
-- Blocker fix): the Ref get-or-create dedup was opened from child-only
-- (ParentID != "") to root tasks too, but idx_tasks_ref_parent's uniqueness
-- was only (ref, parent_id). Every workspace's root (card) tasks share
-- parent_id = '', so two DIFFERENT workspaces/projects using the same source
-- ref (jira issue_key / slack thread_ts / mail message-id — plausible for
-- coincidentally-overlapping ids across unrelated projects) would collide:
-- the second workspace's "create" would silently return the FIRST
-- workspace's task instead of creating its own, a cross-workspace task
-- leak/merge. Scoping uniqueness (and lookup) to also include project_id
-- closes this — see orchestrator.FindTaskByRef's updated signature. This is
-- a strict narrowing (more specific key), so it cannot introduce a NEW
-- collision for any row that previously deduped correctly under
-- (ref, parent_id) — those rows already shared project_id too.
DROP INDEX IF EXISTS idx_tasks_ref_parent;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_ref_parent_project ON tasks(ref, parent_id, project_id) WHERE ref != '';
