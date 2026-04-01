-- boid schema

CREATE TABLE IF NOT EXISTS projects (
    id         TEXT PRIMARY KEY,
    work_dir   TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS project_workspaces (
    project_id    TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    workspace_id  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_project_workspaces_workspace_id
    ON project_workspaces(workspace_id);

CREATE TABLE IF NOT EXISTS tasks (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES projects(id),
    remote_id     TEXT NOT NULL DEFAULT '',
    datasource_id TEXT NOT NULL DEFAULT '',
    title         TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending',
    behavior      TEXT NOT NULL,
    payload       TEXT NOT NULL DEFAULT '{}',
    created_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at    DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_remote
    ON tasks(remote_id, datasource_id)
    WHERE remote_id != '' AND datasource_id != '';

CREATE TABLE IF NOT EXISTS actions (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES tasks(id),
    type       TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS secrets (
    id              TEXT PRIMARY KEY,
    key             TEXT NOT NULL UNIQUE,
    value_encrypted BLOB NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at      DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS jobs (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES tasks(id),
    project_id TEXT NOT NULL REFERENCES projects(id),
    handler_id TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'hook',
    status     TEXT NOT NULL DEFAULT 'running',
    exit_code  INTEGER,
    output     TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS worktrees (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL UNIQUE REFERENCES tasks(id),
    project_id  TEXT NOT NULL REFERENCES projects(id),
    path        TEXT NOT NULL,
    branch      TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
    cleaned_at  DATETIME
);
