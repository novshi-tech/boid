-- docs/plans/card-model-cleanup.md PR-2 (§6.2): fold task_triage into tasks
-- as a Single Table Inheritance table, with a `type` discriminator column
-- ('card' | 'execution') and a CHECK constraint that pins each subtype's
-- field regime and status vocabulary. Runs as ONE transaction (SQLite wraps
-- each migration file in its own tx already, applyMigration in migrate.go) —
-- table rebuild + data migration + drop all commit or roll back together.
--
-- Step 1: build the new table under a temp name.
--
-- worktree stays untouched (still dead, still NOT NULL DEFAULT FALSE — see
-- store.go's own comment on the column; out of scope for this migration,
-- carried over as-is). datasource_id is NOT carried over: migration 0025
-- already dropped it from tasks, so §3.2's "監査して drop 候補:
-- datasource_id" is moot by the time this migration runs — there is nothing
-- left to drop. remote_id stays in the common core (NOT the "監査して決める"
-- Execution side): internal/orchestrator/store.go's UpdateTask doc comment
-- states production fact "khi patches remote_id on working/parked tasks"
-- (working/parked are card-only statuses), and the Web UI's task-edit form
-- (internal/api/web.go's PostTaskEdit) exposes a remote_id field gated only
-- by IsPreDispatchEditableStatus — which admits parked. Both are evidence a
-- card legitimately carries remote_id in production today; moving it to
-- Execution-only would 400 that exact write path. See docs/plans/
-- card-model-cleanup.md §9 for the full decision writeup.
CREATE TABLE tasks_new (
    id            TEXT PRIMARY KEY,
    type          TEXT NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects(id),
    remote_id     TEXT NOT NULL DEFAULT '',
    title         TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending',
    ref           TEXT NOT NULL DEFAULT '',
    parent_id     TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    worktree      BOOLEAN NOT NULL DEFAULT FALSE,

    -- ExecAttrs (design doc §3.2/§3.5). NULL on every card row. behavior is
    -- allowed to be '' (a no-yaml project's resolved behavior name) but NOT
    -- NULL on an execution row — NULL means "not an execution task" here,
    -- not "no behavior" (§3.4's note on this exact distinction).
    behavior      TEXT,
    traits        TEXT,
    readonly      BOOLEAN,
    branch_prefix TEXT,
    base_branch   TEXT,
    payload       TEXT,
    instructions  TEXT,
    auto_start    BOOLEAN,

    -- CardAttrs (design doc §3.2/§3.5). NULL on every execution row.
    -- wake_at is the one card column that may legitimately be NULL even on
    -- a live card row (nil = no date-based wake condition) — it does not
    -- get an "IS NOT NULL for card rows" CHECK below, unlike its siblings.
    kind             TEXT,
    urgency          TEXT,
    wake_at          DATETIME,
    wake_task_id     TEXT,
    suggestion_verb  TEXT,
    detail           TEXT,

    CHECK (type IN ('card', 'execution')),

    -- 型ごとの status 語彙 (§3.4).
    CHECK (type != 'card'      OR status IN ('parked', 'working', 'done', 'dropped')),
    CHECK (type != 'execution' OR status IN ('pending', 'executing', 'awaiting', 'done', 'aborted')),

    -- 相手型のフィールドは NULL (§3.4, 実列全部に張る).
    CHECK (type != 'card' OR (
        behavior IS NULL AND traits IS NULL AND readonly IS NULL AND
        branch_prefix IS NULL AND base_branch IS NULL AND payload IS NULL AND
        instructions IS NULL AND auto_start IS NULL
    )),
    CHECK (type != 'execution' OR (
        kind IS NULL AND urgency IS NULL AND wake_at IS NULL AND
        wake_task_id IS NULL AND suggestion_verb IS NULL AND detail IS NULL
    )),

    -- 自分の型のフィールドは（wake_at を除いて）NULL であってはならない —
    -- 上の CHECK の裏面。behavior だけ空文字が正当 (上のコメント参照) なので
    -- IS NOT NULL のみを課し、値の中身は問わない。
    CHECK (type != 'execution' OR (
        behavior IS NOT NULL AND traits IS NOT NULL AND readonly IS NOT NULL AND
        branch_prefix IS NOT NULL AND base_branch IS NOT NULL AND payload IS NOT NULL AND
        instructions IS NOT NULL AND auto_start IS NOT NULL
    )),
    CHECK (type != 'card' OR (
        kind IS NOT NULL AND urgency IS NOT NULL AND
        wake_task_id IS NOT NULL AND suggestion_verb IS NOT NULL AND detail IS NOT NULL
    ))
);

-- Step 2: migrate every existing task row.
--
-- `classified` computes is_card ONCE per row (referenced by every output
-- column below) instead of repeating the predicate — the type判定の優先順位
-- (§6.2-2) is:
--   1. task_triage に row がある -> card (status は問わない)
--      (PR-2 レビュー注記: task_triage 行はあるが status が execution 専用語彙
--      という組み合わせは理論上ここで is_card=1・execution 専用 status のまま
--      classified に残り、下の CHECK 制約 (status IN ('parked','working',
--      'done','dropped')) に違反して migration 自体が transaction ごと失敗
--      する — 洗い替え対象にしていない以上、is_card=1 のまま素通しするのが
--      唯一の安全な挙動で、想定外の組み合わせをサイレント通過させるより
--      migration を止めて気づける方が正しい。§6.4 のコード監査と現行実装
--      (task_triage 行は常に card 系 status 生成と対で作られ、card 機械の
--      どの遷移も execution 専用 status には到達しない) からは到達不能なはず
--      だが、本番データでの確証はない。もし実際に踏んだ場合は個別 backfill
--      で対処する — 洗い替え規則を広げて機械的に握り潰さないこと)
--   2. row が無くても status が card 系
--      (captured/triaged/parked/ready/working/dropped) -> card
--      (rowless card 救済の最終回 — 実測: api.machineFor の
--      hasTaskTriageRow/isCardLifecycleStatus フォールバックが実際に本番で
--      辿られてきた経路そのもの。SeedTaskTriage は best-effort だったので、
--      rowless card は実在しうる)
--   3. それ以外 -> execution
--
-- captured/triaged/ready は card 側でのみ parked へ 洗い替え (§3.3/§6.2-3):
-- 非終端なので parked が正しい着地。captured/triaged/ready はどのみち上の
-- "card 系 status" リストに含まれるため、この3値を持つ行は必ず is_card=1に
-- 分類される — status 洗い替えの条件式で is_card を重ねて確認する必要は
-- ないが、意図を読みやすくするため明示的に書く。actions 履歴
-- (from_status/to_status) は一切書き換えない — action ログは台帳。
WITH classified AS (
    SELECT
        t.*,
        tt.kind AS tt_kind,
        tt.urgency AS tt_urgency,
        tt.wake_at AS tt_wake_at,
        tt.wake_task_id AS tt_wake_task_id,
        tt.suggestion_verb AS tt_suggestion_verb,
        tt.detail AS tt_detail,
        CASE
            WHEN tt.task_id IS NOT NULL THEN 1
            WHEN t.status IN ('captured', 'triaged', 'parked', 'ready', 'working', 'dropped') THEN 1
            ELSE 0
        END AS is_card
    FROM tasks t
    LEFT JOIN task_triage tt ON tt.task_id = t.id
)
INSERT INTO tasks_new (
    id, type, project_id, remote_id, title, description, status, ref, parent_id,
    created_at, updated_at, worktree,
    behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start,
    kind, urgency, wake_at, wake_task_id, suggestion_verb, detail
)
SELECT
    id,
    CASE WHEN is_card THEN 'card' ELSE 'execution' END,
    project_id,
    remote_id,
    title,
    description,
    CASE WHEN is_card AND status IN ('captured', 'triaged', 'ready') THEN 'parked' ELSE status END,
    ref,
    parent_id,
    created_at,
    updated_at,
    worktree,
    CASE WHEN is_card THEN NULL ELSE behavior END,
    CASE WHEN is_card THEN NULL ELSE traits END,
    CASE WHEN is_card THEN NULL ELSE readonly END,
    CASE WHEN is_card THEN NULL ELSE branch_prefix END,
    CASE WHEN is_card THEN NULL ELSE base_branch END,
    CASE WHEN is_card THEN NULL ELSE payload END,
    CASE WHEN is_card THEN NULL ELSE instructions END,
    CASE WHEN is_card THEN NULL ELSE auto_start END,
    CASE WHEN NOT is_card THEN NULL WHEN tt_kind IS NOT NULL THEN tt_kind ELSE '' END,
    CASE WHEN NOT is_card THEN NULL WHEN tt_urgency IS NOT NULL THEN tt_urgency ELSE '' END,
    CASE WHEN is_card THEN tt_wake_at ELSE NULL END,
    CASE WHEN NOT is_card THEN NULL WHEN tt_wake_task_id IS NOT NULL THEN tt_wake_task_id ELSE '' END,
    CASE WHEN NOT is_card THEN NULL WHEN tt_suggestion_verb IS NOT NULL THEN tt_suggestion_verb ELSE '' END,
    CASE WHEN NOT is_card THEN NULL WHEN tt_detail IS NOT NULL THEN tt_detail ELSE '{}' END
FROM classified;

-- Step 3: swap the tables.
DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;

-- Step 4: recreate the indexes migrations 0037/0035 put on the old shapes
-- (idx_tasks_ref_parent_project on tasks itself, idx_task_triage_wake_at on
-- task_triage) against the new single table. Names changed (task_triage's
-- namesake table is gone) but the predicate is identical: "does this card
-- have a date-based wake condition".
CREATE UNIQUE INDEX idx_tasks_ref_parent_project ON tasks(ref, parent_id, project_id) WHERE ref != '';
CREATE INDEX idx_tasks_wake_at ON tasks(wake_at) WHERE wake_at IS NOT NULL;

-- Step 5: task_triage is fully folded in — drop it. 0035/0040/0044 (the
-- migrations that created and evolved it) stay in this file as history;
-- migrations are append-only, never rewritten.
DROP TABLE task_triage;
