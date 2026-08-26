-- docs/plans/signal-ingest-detailed-design.md §8 (独立 PR): `boid task create
-- --idempotency-key` の永続化列。外部 identity を持たない task (典型は判断が
-- 立てる子 task) の重複生成防止 — task_identities (migration 0041) の identity
-- link とは別の口 (design doc §8「既存の task_resolve_or_capture との違い」:
-- identity の drop 解放/reopen 再燃の意味論は一切持ち込まない、workspace 内部の
-- 安定キーであることが違い)。
--
-- NULL 許容 (NOT NULL DEFAULT '' にしない理由): ref (migration 0010/0037、
-- NOT NULL DEFAULT '' + `WHERE ref != ''` の部分 index) と違い、
-- idempotency_key は「キー無し」を空文字列と共有させない。「キー無し」を
-- NULL 専用の語彙として使うことで、部分 index の対象範囲 (WHERE
-- idempotency_key IS NOT NULL) と Go 側の「空文字列 = 未指定」判定が
-- ズレる余地を無くす。
--
-- 部分 unique index (project_id, idempotency_key) WHERE idempotency_key IS
-- NOT NULL: project スコープ (task_identities/ref と同じ思想 — 別 project が
-- 偶然同じ内部キーを使っても衝突しない)。衝突時の挙動はアプリ側
-- (internal/orchestrator/store.go の CreateTask) が担う — エラーではなく
-- 既存 task の id を返して exit 0 (「再実行が収束する」の実装、ref の
-- get-or-create と対称)。
ALTER TABLE tasks ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_project_idempotency_key
  ON tasks(project_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
