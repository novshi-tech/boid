-- docs/plans/cross-project-issue-triage.md Phase 1 PR-3: queue の決定論的評価
-- 節 rule 7 (沈黙の検知 / watchdog) の primitive。workspace ごとに「最終
-- ingestion 成功」「最終棚卸し実施」の時刻だけを持つ — 評価規則そのもの
-- (閾値超過判定) は Go 側の純粋関数 (orchestrator.WatchdogGuidance) が持ち、
-- ここは時刻の記録だけを担う (決定12: daemon は時計 + DB のみを見る)。
--
-- ingestion (PR-4 スコープ) はまだ存在しないため、last_ingest_success_at の
-- 実際の書き手はまだ無い。テーブルと primitive だけを PR-3 で先置きし、
-- PR-4 の ingestion task がここに書き込む形を見越す。
CREATE TABLE IF NOT EXISTS workspace_watchdog (
    workspace_id            TEXT PRIMARY KEY REFERENCES workspaces(slug) ON DELETE CASCADE,
    last_ingest_success_at  DATETIME,
    last_triage_review_at   DATETIME,
    updated_at              DATETIME NOT NULL
);
