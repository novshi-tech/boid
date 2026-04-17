package orchestrator

import (
	"encoding/json"
	"strings"
)

// ComputeTaskBlocked は task が pending 状態かつ依存条件が未充足のとき true を返す。
// taskByID は全ての関連タスクを ID でひけるマップ。
// 依存先 ID がマップに存在しない場合（削除済み等）はブロックとみなさない。
func ComputeTaskBlocked(task *Task, taskByID map[string]*Task) bool {
	if task.Status != TaskStatusPending {
		return false
	}
	for _, depID := range task.DependsOn {
		dep, ok := taskByID[depID]
		if !ok {
			// 依存先が存在しない（削除済み等）→ブロックとみなさない
			continue
		}
		if dep.Status != TaskStatusDone {
			return true
		}
		// 依存先が done でも DependsOnPayload 条件が未充足ならブロック
		if task.DependsOnPayload != "" {
			v, _ := nestedPayloadGet(dep.Payload, task.DependsOnPayload)
			if !isTruthyVal(v) {
				return true
			}
		}
	}
	return false
}

// nestedPayloadGet は JSON ペイロードからドット区切りキーで値を取り出す。
// 例: "artifact.auto-merge.merged"
func nestedPayloadGet(payload json.RawMessage, key string) (any, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	segments := strings.Split(key, ".")
	var cur any = m
	for _, seg := range segments {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, nil
		}
		cur = mm[seg]
	}
	return cur, nil
}

// isTruthyVal は JSON 値が truthy かどうかを返す。
// null, false, 0, "", [], {} は falsy。
func isTruthyVal(v any) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case float64:
		return val != 0
	case string:
		return val != ""
	case []any:
		return len(val) != 0
	case map[string]any:
		return len(val) != 0
	}
	return true
}
