package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// checkDependencies は task の DependsOn / DependsOnPayload 制約を検証する。
// 条件未充足の場合は具体的な理由を含むエラーを返す。
func checkDependencies(task *orchestrator.Task, getTask func(string) (*orchestrator.Task, error)) error {
	if len(task.DependsOn) == 0 {
		return nil
	}
	for _, depID := range task.DependsOn {
		dep, err := getTask(depID)
		if err != nil {
			return fmt.Errorf("dependency %s: %w", depID, err)
		}
		if dep.Status != orchestrator.TaskStatusDone {
			return fmt.Errorf("dependency %s is not done (status: %s)", depID, dep.Status)
		}
		if task.DependsOnPayload != "" {
			v, err := orchestrator.ResolvePayloadValue(dep, task.DependsOnPayload)
			if err != nil || !isTruthy(v) {
				return fmt.Errorf("dependency %s: payload[%q] is not truthy", depID, task.DependsOnPayload)
			}
		}
	}
	return nil
}

// payloadGet は JSON ペイロードから指定キーの値を取り出す。
// key はドット区切りのネストパス（例: "artifact.auto-merge.merged"）を受け付ける。
func payloadGet(payload json.RawMessage, key string) (any, error) {
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

// isTruthy は JSON 値が truthy かどうかを返す。
// null, false, 0, "", [], {} は falsy。
func isTruthy(v any) bool {
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
