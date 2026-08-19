-- docs/plans/ingestion-identity.md PR-4 (B-5): トリガ実行機構の台帳。
--
-- daemon が持つ 3 つの持ち分 (J-4: スケジュール / single-flight / 実行結果の
-- 記録) のうち、後ろ2つがこのテーブル 1 本に載る (12 節 B-5 既定案「トリガの
-- 実行記録をどう持つか」→ テーブル 1 本)。
--
-- single-flight は「同じ (project_id, trigger_name) に finished_at IS NULL
-- の行があれば見送る」(PR-4 節)。job_id は daemon が daemon 自身から起こした
-- exec job (api.ExecDispatcher.StartExec) の jobs.id — job の完了は
-- daemon が能動的に jobs テーブルを読み戻して検知し (poll、コールバック機構
-- は無い)、finished_at/exit_code をこの行に書き戻す。daemon が再起動して
-- finished_at が NULL のまま残った行は「daemon にしか正しく実装できない」
-- (J-4) はずの single-flight がこの1点で壊れうる残留リスクである —
-- internal/api/trigger_loop.go の SweepTriggers 冒頭の再照合パス
-- (jobs テーブルを引き直し、対応する job が既に終端状態なら finished_at を
-- 埋める。job 行そのものが見当たらない場合も終端側へフェイルクローズする)
-- が、次の tick で必ずこの残留を解消する — 詳細は同ファイルのコメントと
-- このPRの報告を参照。
--
-- FOREIGN KEY ... ON DELETE CASCADE: project 削除時に trigger_runs 行も
-- 一緒に消える。jobs テーブルの project_id 列と同じ制約形なので、cascade を
-- 張らない場合は DeleteProject (project_catalog.go) に trigger_runs の
-- 明示 DELETE を足す必要が生じる — task_identities (migration 0041) が
-- tasks(id) に ON DELETE CASCADE を張っているのと同じ判断で、こちらも
-- cascade を選び、DeleteProject 側の変更を避けた。
CREATE TABLE IF NOT EXISTS trigger_runs (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trigger_name TEXT NOT NULL,
    job_id       TEXT NOT NULL,
    started_at   DATETIME NOT NULL,
    finished_at  DATETIME,
    exit_code    INTEGER
);

-- 毎 tick の single-flight 判定 ("(project_id, trigger_name) に
-- finished_at IS NULL の行があるか") と、直近実行の elapsed 判定
-- ("(project_id, trigger_name) の最新 started_at") の両方がこの複合 index
-- 1 本で引ける。
CREATE INDEX IF NOT EXISTS idx_trigger_runs_project_trigger_started
    ON trigger_runs(project_id, trigger_name, started_at);
