-- docs/plans/cross-project-issue-triage.md Phase 1 PR-1: task_triage sidecar。
-- project_workspaces (0001_initial.sql) と同型の 1:1 sidecar (task_id が PK かつ FK)。
-- urgency/wake_at/wake_task_id は queue の決定論的評価 (決定12) の SQL 述語になるため実列、
-- summary/source/content_ref/children/suggestion/observed は述語にならないので detail の
-- JSON 1 列に集約する (実測c)。orchestrator.Task / TaskService / TaskStore には含めない —
-- Task は DTO を兼ねており列を足すと全 API/CLI/Web の JSON に自動露出するため。
CREATE TABLE IF NOT EXISTS task_triage (
    task_id      TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL DEFAULT '',   -- signal|issue|theme (theme はlifecycleに乗らない)
    urgency      TEXT NOT NULL DEFAULT '',   -- now|today|week|someday (queue述語, 決定12)
    wake_at      DATETIME,                   -- parked復帰の日時条件。NULL = 日時wakeなし
    wake_task_id TEXT NOT NULL DEFAULT '',   -- 事象wake: このtaskが終端に達したら復帰
    detail       TEXT NOT NULL DEFAULT '{}'  -- summary/source/content_ref/children/suggestion/observed
);
CREATE INDEX IF NOT EXISTS idx_task_triage_wake_at
    ON task_triage(wake_at) WHERE wake_at IS NOT NULL;
