-- docs/plans/ingestion-identity.md PR-1 (B-1): identity 索引。
-- 外部キー (`jira:ROOKPF-289` / `slack:1785803139.540489` 等) から task を
-- 引けるようにする索引テーブル。認証の identity → principal と同じ構造で、
-- task が principal、外部キーが identity にあたる (I-1)。
--
-- UNIQUE (project_id, identity) が不変条件の核: 1 つの identity は project
-- スコープで高々 1 つの task にしか紐付かない (I-1)。逆に task 側は複数の
-- identity を持てるので task_id には UNIQUE を張らない。project スコープは
-- I-3 — 取り込まれるメタタスクはメタプロジェクトにしか紐づかないため、
-- migration 0037 の idx_tasks_ref_parent_project と同じスコープを踏襲する。
--
-- identity は `<namespace>:<key>` の不透明文字列として扱う (I-2)。daemon は
-- 中身を一切解釈しない — namespace の意味も key の妥当性も workspace 側の
-- 知識であり、ここでは「空でないこと」以外の検証をしない。
--
-- INDEX (task_id) は逆引き用 (task の identity 一覧、drop 時の一括解放)。
--
-- FOREIGN KEY ... ON DELETE CASCADE: 終端 task の 30 日 GC (GCTasks) が
-- `DELETE FROM tasks` するだけで binding も一緒に消える。I-5 (done では
-- identity を握り続ける) は、この cascade により実質 30 日の期限付きになる
-- が、task 行が消えた後も binding だけ残す意味が無いため受容する
-- (設計 doc 10 節 B-1 の中で決めた既定案)。
--
-- tasks.ref には一切触らない。ref は子 task の dedup 専用として残り、外部
-- キー用途だけがこちらへ移る (I-7)。root task の ref を空にする migration
-- は、PR-2 の宛先解決切り替えが実地で回ったことを確認してから別途出す。
CREATE TABLE IF NOT EXISTS task_identities (
    identity   TEXT NOT NULL,
    project_id TEXT NOT NULL,
    task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL,
    UNIQUE (project_id, identity)
);
CREATE INDEX IF NOT EXISTS idx_task_identities_task_id ON task_identities(task_id);
