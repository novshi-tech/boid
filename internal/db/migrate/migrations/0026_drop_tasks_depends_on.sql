DROP TABLE IF EXISTS task_dependencies;
ALTER TABLE tasks DROP COLUMN depends_on_payload;
