-- docs/plans/ingestion-identity.md PR-4 (B-5): トリガ実行機構の台帳。
--
-- daemon が持つ 3 つの持ち分 (J-4: スケジュール / single-flight / 実行結果の
-- 記録) のうち、後ろ2つがこのテーブル 1 本に載る (12 節 B-5 既定案「トリガの
-- 実行記録をどう持つか」→ テーブル 1 本)。
--
-- single-flight は「同じ (project_id, trigger_name) に finished_at IS NULL
-- の行があれば見送る」(PR-4 節)。この不変条件は **DB 自身の部分 UNIQUE
-- インデックス** (下記 idx_trigger_runs_inflight_unique) が守る — アプリ側の
-- 「読んでから書く」だけに頼らない。理由 (Opus レビュー Blocker 1,
-- PR-4 実装後 2026-08-19): 列挙 (ListInFlightTriggerRuns) → dispatch
-- (StartExec、コンテナ起動で実機数秒かかる) → 記録 (旧: CreateTriggerRun)
-- がすべて別トランザクションで、`internal/db/db.go` の SetMaxOpenConns(1)
-- は「1 文ずつ」を直列化するだけで「読む→コンテナを起こす→書く」という
-- Go 関数レベルの一連の流れは一切守らない。二重 dispatch は
-- (a) 手動実行と sweep tick の競合、(b) 手動実行の連打、(c) daemon
-- プロセスが複数、のいずれでも実際に再現した。
--
-- 対策は「行を先に INSERT してから dispatch する」への並び替え —
-- internal/api/trigger_loop.go の fireTrigger を参照。INSERT が
-- idx_trigger_runs_inflight_unique の制約違反で弾かれる = 既に in-flight
-- という意味になり、これは同一プロセス内の複数 goroutine だけでなく
-- **複数 daemon プロセスが同じ DB file を共有するケースでも効く**
-- (J-4 が「workspace 側の flock はホスト単位でしか効かず、コンテナ化された
-- daemon や複数ホストで破れる」と書いていた優位性が、この実装で裏付けられる)。
--
-- job_id は dispatch 成功後に UPDATE で埋める (internal/orchestrator/
-- trigger_run.go の SetTriggerRunJobID) ため、INSERT 時点ではまだ空文字列
-- でありうる — NOT NULL のまま DEFAULT '' にしているのはそのため。
-- dispatch 自体が失敗した場合はその行を DeleteTriggerRun で削除する
-- (「dispatch 失敗はフェイルオープンで即リトライ」という既存の不変条件を
-- 保つため — 行を残して CompleteTriggerRun で閉じる形にすると、
-- 直後の再試行が「every 未経過」判定に化けてしまう)。
--
-- job 行そのものが見当たらない場合 (30 日 GC で taskless job が消えた等)
-- は、internal/api/trigger_loop.go の reconcileInFlight がまず「次の tick
-- まで in-flight のまま残す」形でリトライを試み (transient なエラーと
-- 区別できないため)、その状態が TriggerRunSelfHealGrace を超えて続いた
-- 場合にのみ自己回復として終端側へフェイルクローズする (N-1, Opus レビュー
-- — このコメントは旧版で「フェイルクローズする」とだけ書いていたが、
-- 実装 (trigger_loop.go の jobTerminalState 自体) はエラーを即終端扱いには
-- しない。両者が矛盾していたのを直した)。
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
    job_id       TEXT NOT NULL DEFAULT '',
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

-- single-flight 本体: (project_id, trigger_name) の組は in-flight
-- (finished_at IS NULL) の行を高々 1 つしか持てない。部分 UNIQUE
-- インデックスなので、完了済み行 (finished_at 有り) は何個あっても
-- 制約に触れない — 履歴は無制限に積み上がる (N-2 の GC 対象)。
CREATE UNIQUE INDEX IF NOT EXISTS idx_trigger_runs_inflight_unique
    ON trigger_runs(project_id, trigger_name) WHERE finished_at IS NULL;

-- N-2 (Opus レビュー): ListInFlightTriggerRuns の
-- `WHERE finished_at IS NULL ORDER BY started_at ASC, id ASC` を専用にカバー
-- する部分 index。これが無いと EXPLAIN QUERY PLAN が実測で
-- `SCAN trigger_runs` + `USE TEMP B-TREE FOR ORDER BY` になり、行数が
-- 増える (retention が無ければ khi の 2 トリガだけで ~105,000 行/年) につれ
-- 毎分 (sweep tick 単位) のフルスキャン + 一時ソートになる。
-- idx_trigger_runs_inflight_unique 自体は (project_id, trigger_name) 順
-- なので、この「全 project 横断で started_at 順」というクエリ形には
-- 直接使えない — 別の部分 index が要る。
CREATE INDEX IF NOT EXISTS idx_trigger_runs_inflight_started
    ON trigger_runs(started_at, id) WHERE finished_at IS NULL;
