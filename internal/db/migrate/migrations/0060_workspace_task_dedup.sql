CREATE UNIQUE INDEX idx_tasks_workspace_ref
    ON tasks(workspace_id, parent_id, ref) WHERE project_id IS NULL AND ref != '';
CREATE UNIQUE INDEX idx_tasks_workspace_idempotency_key
    ON tasks(workspace_id, parent_id, idempotency_key) WHERE project_id IS NULL AND idempotency_key IS NOT NULL;
