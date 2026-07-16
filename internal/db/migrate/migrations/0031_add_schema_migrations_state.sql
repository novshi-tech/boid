-- docs/plans/workspace-db-consolidation.md: schema_migrations 拡張 (PR3 前段)。
-- workspace_db_consolidation migration の staging/committed 二段階 state を
-- 記録できるようにする。 既存行は state='committed' (後方互換、これまでの
-- 全ての file-based migration は「実行完了済み」の意味で扱う) / input_hash=''
-- (workspace_db_consolidation 以外は input_hash を使わない) で埋まる。
ALTER TABLE schema_migrations ADD COLUMN state TEXT NOT NULL DEFAULT 'committed';
ALTER TABLE schema_migrations ADD COLUMN input_hash TEXT NOT NULL DEFAULT '';
