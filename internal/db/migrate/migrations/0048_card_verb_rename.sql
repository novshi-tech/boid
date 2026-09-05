-- docs/plans/card-next-step-and-timeline.md §3.1/§8: card-lifecycle verbs
-- `working`/`done` renamed to `start`/`complete`. execution 型の同名の
-- start/done は無関係 — このデータ backfill は type='card' の行だけを対象と
-- する。past action 履歴 (actions.type / actions.from_status / to_status) は
-- 台帳なので書き換えない。書き換えるのは「現在保存されている suggestion」
-- (§8) だけ: task_triage 相当の列 (suggestion_verb) と detail JSON の
-- suggestion.verb フィールド。

-- 昇格列 (0044 で追加、migration 0045 で tasks へ統合済み)。
UPDATE tasks
SET suggestion_verb = CASE suggestion_verb
    WHEN 'working' THEN 'start'
    WHEN 'done' THEN 'complete'
    ELSE suggestion_verb
END
WHERE type = 'card' AND suggestion_verb IN ('working', 'done');

-- detail.attrs.suggestion.verb — attrs_set の書き込み先 (実運用で実際に
-- 使われている経路。internal/orchestrator/card.go の DetailSuggestion 参照)。
UPDATE tasks
SET detail = json_set(
    detail, '$.attrs.suggestion.verb',
    CASE json_extract(detail, '$.attrs.suggestion.verb')
        WHEN 'working' THEN 'start'
        WHEN 'done' THEN 'complete'
    END
)
WHERE type = 'card'
  AND json_valid(detail)
  AND json_extract(detail, '$.attrs.suggestion.verb') IN ('working', 'done');

-- detail.suggestion.verb (top-level) — DetailSuggestion がフォールバック
-- 先として読む代替の置き場所。実運用では未使用の経路だが、将来/別 writer が
-- ここに直接書いていた場合の取りこぼしを防ぐ。
UPDATE tasks
SET detail = json_set(
    detail, '$.suggestion.verb',
    CASE json_extract(detail, '$.suggestion.verb')
        WHEN 'working' THEN 'start'
        WHEN 'done' THEN 'complete'
    END
)
WHERE type = 'card'
  AND json_valid(detail)
  AND json_extract(detail, '$.suggestion.verb') IN ('working', 'done');
