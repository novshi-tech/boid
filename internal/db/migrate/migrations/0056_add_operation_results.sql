CREATE TABLE operation_results (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    operation_type TEXT NOT NULL,
    operation_label TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('accepted', 'started', 'rejected', 'unknown')),
    reason_code TEXT NOT NULL,
    target_task_id TEXT NOT NULL DEFAULT '',
    target_session_id TEXT NOT NULL DEFAULT '',
    target_request_id TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_operation_results_task_created
    ON operation_results(task_id, created_at DESC, id DESC);

CREATE INDEX idx_operation_results_created
    ON operation_results(created_at);
