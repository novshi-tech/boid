-- card_requests: card 上の一つの実行要求 (人発コマンド/Go、または内部
-- イベント発の自動要求) を1行として持つ台帳。
--
-- 概念状態: queued → launching → attached → finished/failed。folded は
-- 複数の queued 行を一回の起動にまとめたときの、代表に選ばれなかった行の
-- 一時状態で、代表行の結果に応じて finished か queued へ移る
-- (internal/orchestrator/card_request.go)。
--
-- card ごとに launching/attached の行は高々一つ、という不変条件は
-- idx_card_requests_active_unique の部分 UNIQUE インデックスが保証する。
--
-- command_key: '' は Go (作業実行由来) を表す予約値。card_commands の
-- キーは空文字列を許さないので実在キーと衝突しない。
--
-- cause_id: 内部イベント発の要求が持つ原因 ID。idx_card_requests_cause_unique
-- が同じ原因の再配達を重複排除する。人発の要求は '' のままで衝突しない。
--
-- launched_* 列は queued→launching 遷移の瞬間に card_commands 定義を
-- スナップショットしたもの (queued の間は空文字列)。
--
-- folded_into: 代表として起動された行の id。
--
-- target_kind/target_id: launcher が作った継続先 (task か session) への
-- 関連。launcher_job_id は launcher 自身の exec job で、継続先とは別物。
CREATE TABLE IF NOT EXISTS card_requests (
    id                    TEXT PRIMARY KEY,
    card_id               TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    command_key           TEXT NOT NULL DEFAULT '',
    cause_id              TEXT NOT NULL DEFAULT '',
    status                TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'launching', 'attached', 'folded', 'finished', 'failed')),
    launched_command_key  TEXT NOT NULL DEFAULT '',
    launched_label        TEXT NOT NULL DEFAULT '',
    launched_run          TEXT NOT NULL DEFAULT '',
    launched_version      TEXT NOT NULL DEFAULT '',
    launcher_job_id       TEXT NOT NULL DEFAULT '',
    target_kind           TEXT NOT NULL DEFAULT '' CHECK (target_kind IN ('', 'task', 'session')),
    target_id             TEXT NOT NULL DEFAULT '',
    folded_into           TEXT NOT NULL DEFAULT '',
    result                TEXT NOT NULL DEFAULT '',
    error                 TEXT NOT NULL DEFAULT '',
    created_at            DATETIME NOT NULL,
    updated_at            DATETIME NOT NULL
);

-- card_id ごとに launching/attached は高々一つ。
CREATE UNIQUE INDEX IF NOT EXISTS idx_card_requests_active_unique
    ON card_requests(card_id) WHERE status IN ('launching', 'attached');

-- 原因 ID の再配達重複排除。'' は「原因 ID なし」の予約値なので対象外。
CREATE UNIQUE INDEX IF NOT EXISTS idx_card_requests_cause_unique
    ON card_requests(cause_id) WHERE cause_id != '';

-- ClaimQueuedCardRequests の "card_id の queued 行を created_at 順に取る"
-- クエリと、一般的な「この card の要求一覧」表示の両方をカバーする。
CREATE INDEX IF NOT EXISTS idx_card_requests_card_status_created
    ON card_requests(card_id, status, created_at);

-- GCCardRequests の "finished/failed かつ updated_at < cutoff" 削除クエリ用。
CREATE INDEX IF NOT EXISTS idx_card_requests_terminal_updated
    ON card_requests(updated_at) WHERE status IN ('finished', 'failed');
