package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ResolveTaskField returns the value at the given dotted path against the task.
//
// Path resolution:
//   - Each dot-separated segment is a JSON key.
//   - The first segment is matched against top-level Task fields (via JSON
//     tags). When it doesn't match, the path is implicitly resolved inside
//     `payload`, so traits like `awaiting.question`, `artifact.report`, and
//     `lifecycle.abort.message` work without an explicit `payload.` prefix.
//   - `lifecycle` is a computed trait derived from action history. When
//     lifecycle is referenced, ResolveTaskField calls DeriveLifecycle (when
//     actions is non-nil) and injects the result under `payload.lifecycle`
//     before traversal.
//
// Output format:
//   - Strings: returned unquoted.
//   - Numbers and booleans: stringified.
//   - Objects and arrays: compact JSON.
//   - Missing path: returns "" with no error.
//   - Traversing into a scalar with remaining path segments: error.
func ResolveTaskField(task *orchestrator.Task, actions orchestrator.LifecycleStore, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("field path is empty")
	}
	if task == nil {
		return "", fmt.Errorf("task is nil")
	}

	raw, err := json.Marshal(task)
	if err != nil {
		return "", fmt.Errorf("marshal task: %w", err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", fmt.Errorf("unmarshal task: %w", err)
	}

	segments := strings.Split(path, ".")

	// Auto-prefix `payload.` when the first segment is not a top-level field.
	if _, ok := top[segments[0]]; !ok {
		segments = append([]string{"payload"}, segments...)
	}

	// Inject lifecycle (computed) when the path enters payload.lifecycle.
	if len(segments) >= 2 && segments[0] == "payload" && segments[1] == "lifecycle" && actions != nil {
		lc, derr := orchestrator.DeriveLifecycle(context.Background(), task.ID, actions, false)
		if derr == nil {
			lcJSON, mErr := json.Marshal(lc)
			if mErr == nil {
				var lcAny any
				if uErr := json.Unmarshal(lcJSON, &lcAny); uErr == nil {
					payload, _ := top["payload"].(map[string]any)
					if payload == nil {
						payload = make(map[string]any)
					}
					payload["lifecycle"] = lcAny
					top["payload"] = payload
				}
			}
		}
	}

	return traverseSegments(top, segments, path)
}

// ResolveJSONField resolves a dotted path against arbitrary already-marshaled
// JSON data, using the same segment-traversal / value-formatting rules as
// ResolveTaskField (missing path → "", scalar → unquoted/stringified,
// object/array → compact JSON, traversing into a scalar → error). Unlike
// ResolveTaskField it does no task-specific implicit `payload.` prefixing or
// `lifecycle` injection — every other Phase 5b PR1 task-context RPC (`boid
// task current` / `instructions` / `env` / `payload`,
// docs/plans/phase5-shim-and-task-context.md) shares this generic core so
// `--field` behaves identically everywhere task_get already established the
// contract.
func ResolveJSONField(raw json.RawMessage, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("field path is empty")
	}
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", fmt.Errorf("unmarshal value: %w", err)
	}
	return traverseSegments(top, strings.Split(path, "."), path)
}

// traverseSegments walks top following segments (JSON object keys), applying
// the shared field-resolution rules used by both ResolveTaskField and
// ResolveJSONField. path is the original (un-split) dotted path, used only
// for error messages.
func traverseSegments(top any, segments []string, path string) (string, error) {
	var cur any = top
	for i, seg := range segments {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return "", nil
			}
			cur = next
		case nil:
			return "", nil
		default:
			return "", fmt.Errorf("cannot traverse into non-object at segment %q (path %q)", strings.Join(segments[:i], "."), path)
		}
	}

	return formatFieldValue(cur)
}

func formatFieldValue(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), nil
		}
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	case json.Number:
		return t.String(), nil
	default:
		out, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("marshal field value: %w", err)
		}
		return string(out), nil
	}
}
