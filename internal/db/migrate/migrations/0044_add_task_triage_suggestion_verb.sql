-- docs/plans/suggestion-as-state-transition-impl.md §4.1 (PR-2): suggestion_verb
-- を task_triage の実列に昇格する。TaskTriage の doc comment
-- (internal/orchestrator/task_triage.go) 自身が書いている原則 —
-- 「queue 述語になる値は列にする」(urgency/wake_at が列である理由と同じ) —
-- を suggestion にも適用する。「一覧は suggestion で駆動する」設計
-- (docs/plans/suggestion-as-state-transition.md §3.6) の queue 述語がこの列だけを
-- 読む (internal/orchestrator/store.go の "queue_next" 分岐)。
--
-- 昇格するのは verb のみ — suggestion 本体 (reason/params) は引き続き
-- detail.attrs.suggestion の JSON blob に残る (表示側は従来通り
-- orchestrator.DetailSuggestion がそちらを読む。列は SQL 述語専用)。
--
-- urgency/kind と同じ TEXT NOT NULL DEFAULT '' 方式を採用した (nullable
-- にしなかった理由): parsePromotedAttr の null→"" 変換と同じ「クリア済み」
-- 表現をこの列でも使い、同じ「閉じた語彙の文字列カラム」である urgency/kind と
-- 挙動を揃える。wake_at のような日時カラムではなく列挙値なので、この2列との
-- 対称性を優先した (実装計画ブリーフの "IS NOT NULL" という表現は設計意図の
-- 要約であって、リテラルな SQL 型を指定したものではないと判断した — PR 本文
-- 参照)。
ALTER TABLE task_triage ADD COLUMN suggestion_verb TEXT NOT NULL DEFAULT '';

-- urgency/kind (migration 0040) と同じ「二段構え」の第一段: 既存行が持つ
-- detail.attrs.suggestion.verb を列に昇格する backfill。第二段 (今後の
-- 書き込みで毎回列へ書く) は internal/api/workflow_triage.go の
-- applyAttrsSetSideEffect が担当する。
--
-- urgency/kind と異なり、verb は blob からは取り除かない: suggestion 本体の
-- 表示 (orchestrator.Suggestion.Verb 経由のバッジ描画等) は今後も blob の
-- suggestion オブジェクト全体を読み続けるため、そこから verb だけを消すと
-- 表示が壊れる。列と blob の両方に verb が乗るのは意図的な二重化であり、
-- fold 側 (applyAttrsSetSideEffect) が両方を同じ書き込みで揃え続けるので
-- drift しない — urgency/kind の「昇格したら blob 側は除去する」規律とは
-- ここが異なる。
-- PR #988 review, MEDIUM 2: 終端 (done/dropped/aborted) な card は backfill 対象から
-- 除外する。カットオーバー runbook (docs/plans/suggestion-as-state-transition-impl.md
-- §6) の手順2は「現 daemon (= PR-1/PR-2 より前、suggestion strip が無い) のまま全 card
-- を drop/done へ終端させる」であり、その時点の直接 drop/done は
-- detail.attrs.suggestion を消さない (strip は PR-1 以降の新機能)。終端させた後の
-- 手順4でこの migration が新 daemon と一緒に初めて走るため、除外しないと「終端させた
-- はずの card が stale な suggestion を理由に suggestion_verb を得て、status 不問に
-- なった queue_next (store.go) の Queue タブに永久に (Reject されるか 30 日 GC される
-- まで) 居座り続ける」という事故になる — 設計 doc §3.6 の「機械が人に判断を求めている
-- ものだけが並ぶ」を初日から破ることになるため、確実に防ぐ。
UPDATE task_triage
SET suggestion_verb = json_extract(detail, '$.attrs.suggestion.verb')
WHERE suggestion_verb = ''
  AND json_valid(detail)
  AND json_extract(detail, '$.attrs.suggestion.verb') IN ('go', 'working', 'park', 'drop', 'done', 'reopen')
  AND task_id IN (SELECT id FROM tasks WHERE status NOT IN ('done', 'dropped', 'aborted'));
