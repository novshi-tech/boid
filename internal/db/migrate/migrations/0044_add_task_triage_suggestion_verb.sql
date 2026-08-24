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
-- PR #988 review, MEDIUM 2 (最初は 終端 done/dropped/aborted の除外リストだった)、
-- unapplicable-suggestion-guard PR review LOW 3 で許可リストに変更: backfill 対象は
-- card 機械 v2 が実際に扱う **parked/working の2状態だけ**にする。
--
-- 除外リスト方式 (`status NOT IN ('done','dropped','aborted')`) だった当初の理由 —
-- カットオーバー runbook (docs/plans/suggestion-as-state-transition-impl.md §6) の
-- 手順2は「現 daemon (= PR-1/PR-2 より前、suggestion strip が無い) のまま全 card を
-- drop/done へ終端させる」であり、その時点の直接 drop/done は detail.attrs.suggestion
-- を消さない (strip は PR-1 以降の新機能)。終端させた後の手順4でこの migration が
-- 新 daemon と一緒に初めて走るため、終端 card を除外しないと「終端させたはずの card が
-- stale な suggestion を理由に suggestion_verb を得て、status 不問になった queue_next
-- (store.go) の Queue タブに永久に居座り続ける」事故になる — ここまでは変わらない。
--
-- だが除外リストは legacy status (captured/triaged/ready) を素通しさせてしまう。
-- これらは card 機械 v2 のどの rule にも登場せず（洗い替えで parked へ作り直される
-- 運命の status）、しかも web/templates/tasks.templ の Accept/Reject ボタンは
-- CanApplyManualAction("answered", status) が真になる4状態 (parked/working/done/
-- dropped) でしか描画されない。つまり legacy status の card に suggestion が
-- 残っていた場合、除外リスト方式では suggestion_verb が backfill されて Queue
-- タブに「適用不能」バッジ付きで出現するのに、Accept も Reject も出せず
-- 30 日 GC まで消せない、という新しい詰みを生む — これは本 PR が塞ごうとしている
-- 症状そのもの。legacy status は backfill する意味自体が無い（洗い替えで消える）ので、
-- 許可リスト方式 (`status IN ('parked','working')`) に倒して構造的に締め出す。
-- 将来 status が増減しても「新設の status は明示的に列挙するまで backfill されない」
-- という安全側に倒れる（除外リストは逆に「見落とした新 status を素通しする」側に
-- 倒れていた）。
UPDATE task_triage
SET suggestion_verb = json_extract(detail, '$.attrs.suggestion.verb')
WHERE suggestion_verb = ''
  AND json_valid(detail)
  AND json_extract(detail, '$.attrs.suggestion.verb') IN ('go', 'working', 'park', 'drop', 'done', 'reopen')
  AND task_id IN (SELECT id FROM tasks WHERE status IN ('parked', 'working'));
