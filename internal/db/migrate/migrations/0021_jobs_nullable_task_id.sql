-- Make jobs.task_id nullable to allow standalone command executions not tied to a task.
-- SQLite requires table recreation to remove NOT NULL; data is preserved as-is.
CREATE TABLE jobs_new (
    id              TEXT PRIMARY KEY,
    task_id         TEXT REFERENCES tasks(id),
    project_id      TEXT NOT NULL REFERENCES projects(id),
    handler_id      TEXT NOT NULL DEFAULT '',
    role            TEXT NOT NULL DEFAULT 'hook',
    runtime_id      TEXT NOT NULL DEFAULT '',
    interactive     INTEGER NOT NULL DEFAULT 0,
    tty             INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'running',
    exit_code       INTEGER,
    output          TEXT NOT NULL DEFAULT '',
    execution_state TEXT NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at      DATETIME NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO jobs_new (id, task_id, project_id, handler_id, role, runtime_id, interactive, tty, status, exit_code, output, execution_state, created_at, updated_at)
SELECT id, NULLIF(task_id, ''), project_id, handler_id, role, runtime_id, interactive, tty, status, exit_code, output, execution_state, created_at, updated_at
FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;
