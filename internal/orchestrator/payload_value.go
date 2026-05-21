package orchestrator

import (
	"encoding/json"
	"strings"
)

// ResolvePayloadValue resolves a key against a task's payload.
// Reserved virtual keys (artifact.children.*) are computed from child count fields.
// All other keys are looked up in the task's payload JSON.
func ResolvePayloadValue(dep *Task, key string) (any, error) {
	switch key {
	case "artifact.children.all_done":
		return dep.TotalChildCount > 0 && dep.DoneChildCount == dep.TotalChildCount, nil
	case "artifact.children.all_resolved":
		return dep.TotalChildCount > 0 && dep.DoneChildCount+dep.AbortedChildCount == dep.TotalChildCount, nil
	default:
		return nestedPayloadGet(dep.Payload, key)
	}
}

// nestedPayloadGet は JSON ペイロードからドット区切りキーで値を取り出す。
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
