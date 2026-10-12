-- Add workspace ownership while preserving existing data and repository constraints.
ALTER TABLE projects ADD COLUMN perm TEXT NOT NULL DEFAULT 'push' CHECK (perm IN ('fetch', 'push'));

CREATE TABLE tasks_new (
    id            TEXT PRIMARY KEY,
    type          TEXT NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT 'default' CHECK (workspace_id != ''),
    project_id    TEXT REFERENCES projects(id),
    remote_id     TEXT NOT NULL DEFAULT '',
    title         TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending',
    ref           TEXT NOT NULL DEFAULT '',
    parent_id     TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    worktree      BOOLEAN NOT NULL DEFAULT FALSE,

    behavior      TEXT,
    traits        TEXT,
    readonly      BOOLEAN,
    branch_prefix TEXT,
    base_branch   TEXT,
    payload       TEXT,
    instructions  TEXT,
    auto_start    BOOLEAN,

    kind             TEXT,
    urgency          TEXT,
    wake_at          DATETIME,
    wake_task_id     TEXT,
    suggestion_verb  TEXT,
    detail           TEXT, idempotency_key TEXT,

    CHECK (type IN ('card', 'execution')),

    CHECK (type != 'card'      OR status IN ('parked', 'working', 'done', 'dropped')),
    CHECK (type != 'execution' OR status IN ('pending', 'executing', 'awaiting', 'done', 'aborted')),

    CHECK (type != 'card' OR (
        behavior IS NULL AND traits IS NULL AND readonly IS NULL AND
        branch_prefix IS NULL AND base_branch IS NULL AND payload IS NULL AND
        instructions IS NULL AND auto_start IS NULL
    )),
    CHECK (type != 'execution' OR (
        kind IS NULL AND urgency IS NULL AND wake_at IS NULL AND
        wake_task_id IS NULL AND suggestion_verb IS NULL AND detail IS NULL
    )),

    CHECK (type != 'execution' OR (
        behavior IS NOT NULL AND traits IS NOT NULL AND readonly IS NOT NULL AND
        branch_prefix IS NOT NULL AND base_branch IS NOT NULL AND payload IS NOT NULL AND
        instructions IS NOT NULL AND auto_start IS NOT NULL
    )),
    CHECK (type != 'card' OR (
        kind IS NOT NULL AND urgency IS NOT NULL AND
        wake_task_id IS NOT NULL AND suggestion_verb IS NOT NULL AND detail IS NOT NULL
    ))
);

INSERT INTO tasks_new (id, type, project_id, remote_id, title, description, status, ref, parent_id, created_at, updated_at, worktree, behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start, kind, urgency, wake_at, wake_task_id, suggestion_verb, detail, idempotency_key, workspace_id)
SELECT r.id, r.type, r.project_id, r.remote_id, r.title, r.description, r.status, r.ref, r.parent_id, r.created_at, r.updated_at, r.worktree, r.behavior, r.traits, r.readonly, r.branch_prefix, r.base_branch, r.payload, r.instructions, r.auto_start, r.kind, r.urgency, r.wake_at, r.wake_task_id, r.suggestion_verb, r.detail, r.idempotency_key,
    COALESCE((SELECT NULLIF(pw.workspace_id, '') FROM project_workspaces pw WHERE pw.project_id = r.project_id), 'default')
FROM tasks r;
DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;

CREATE UNIQUE INDEX idx_tasks_project_parent_idempotency_key
  ON tasks(project_id, parent_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX idx_tasks_ref_parent_project ON tasks(ref, parent_id, project_id) WHERE ref != '';
CREATE INDEX idx_tasks_wake_at ON tasks(wake_at) WHERE wake_at IS NOT NULL;

CREATE TABLE jobs_new (
    id              TEXT PRIMARY KEY,
    task_id         TEXT REFERENCES tasks(id),
    workspace_id TEXT NOT NULL DEFAULT 'default' CHECK (workspace_id != ''),
    project_id      TEXT REFERENCES projects(id),
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
, display_name TEXT NOT NULL DEFAULT '', card_id TEXT NOT NULL DEFAULT '', card_request_id TEXT NOT NULL DEFAULT '', terminal_title TEXT NOT NULL DEFAULT '', display_name_default INTEGER NOT NULL DEFAULT 0);

INSERT INTO jobs_new (id, task_id, project_id, handler_id, role, runtime_id, interactive, tty, status, exit_code, output, execution_state, created_at, updated_at, display_name, card_id, card_request_id, terminal_title, display_name_default, workspace_id)
SELECT r.id, r.task_id, r.project_id, r.handler_id, r.role, r.runtime_id, r.interactive, r.tty, r.status, r.exit_code, r.output, r.execution_state, r.created_at, r.updated_at, r.display_name, r.card_id, r.card_request_id, r.terminal_title, r.display_name_default,
    COALESCE((SELECT NULLIF(pw.workspace_id, '') FROM project_workspaces pw WHERE pw.project_id = r.project_id), 'default')
FROM jobs r;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE INDEX idx_jobs_card_request_id
    ON jobs(card_request_id) WHERE card_request_id != '';

CREATE TABLE trigger_runs_new (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default' CHECK (workspace_id != ''),
    project_id   TEXT REFERENCES projects(id) ON DELETE CASCADE,
    trigger_name TEXT NOT NULL,
    job_id       TEXT NOT NULL DEFAULT '',
    started_at   DATETIME NOT NULL,
    finished_at  DATETIME,
    exit_code    INTEGER
);

INSERT INTO trigger_runs_new (id, project_id, trigger_name, job_id, started_at, finished_at, exit_code, workspace_id)
SELECT r.id, r.project_id, r.trigger_name, r.job_id, r.started_at, r.finished_at, r.exit_code,
    COALESCE((SELECT NULLIF(pw.workspace_id, '') FROM project_workspaces pw WHERE pw.project_id = r.project_id), 'default')
FROM trigger_runs r;
DROP TABLE trigger_runs;
ALTER TABLE trigger_runs_new RENAME TO trigger_runs;

CREATE INDEX idx_trigger_runs_inflight_started
    ON trigger_runs(started_at, id) WHERE finished_at IS NULL;
CREATE UNIQUE INDEX idx_trigger_runs_inflight_unique
    ON trigger_runs(project_id, trigger_name) WHERE finished_at IS NULL;
CREATE INDEX idx_trigger_runs_project_trigger_started
    ON trigger_runs(project_id, trigger_name, started_at);
