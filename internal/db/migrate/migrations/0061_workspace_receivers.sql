CREATE TABLE task_identities_new (
    identity TEXT NOT NULL,
    project_id TEXT,
    workspace_id TEXT NOT NULL CHECK (workspace_id != ''),
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL,
    url TEXT,
    display_name TEXT,
    UNIQUE (project_id, identity)
);
INSERT INTO task_identities_new (identity,project_id,workspace_id,task_id,created_at,url,display_name)
SELECT i.identity,i.project_id,t.workspace_id,i.task_id,i.created_at,i.url,i.display_name
FROM task_identities i JOIN tasks t ON t.id=i.task_id;
DROP TABLE task_identities;
ALTER TABLE task_identities_new RENAME TO task_identities;
CREATE INDEX idx_task_identities_task_id ON task_identities(task_id);
CREATE UNIQUE INDEX idx_task_identities_workspace ON task_identities(workspace_id,identity) WHERE project_id IS NULL;
CREATE UNIQUE INDEX idx_trigger_runs_workspace_inflight ON trigger_runs(workspace_id,trigger_name) WHERE project_id IS NULL AND finished_at IS NULL;
CREATE INDEX idx_trigger_runs_workspace_started ON trigger_runs(workspace_id,trigger_name,started_at) WHERE project_id IS NULL;
