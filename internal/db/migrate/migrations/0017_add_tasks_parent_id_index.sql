CREATE INDEX IF NOT EXISTS idx_tasks_parent_id ON tasks(parent_id) WHERE parent_id != '';
