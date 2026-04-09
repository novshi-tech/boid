ALTER TABLE tasks ADD COLUMN ref TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_ref_parent ON tasks(ref, parent_id) WHERE ref != '';
