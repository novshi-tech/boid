package orchestrator

import (
	"encoding/json"
	"strings"
)

// ResolvePayloadValue resolves a depends_on_payload key against a dependency task.
// Reserved virtual keys (artifact.children.*) are computed from the task's child
// count fields. All other keys are looked up in the task's payload JSON.
// Kept for api/depends_on_check.go compatibility; removed in child 2/3.
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
