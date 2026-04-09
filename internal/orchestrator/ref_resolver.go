package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/uuid"
)

// payloadTaskSpec is the JSON representation of a task in payload_patch.tasks.
type payloadTaskSpec struct {
	Title            string   `json:"title"`
	Description      string   `json:"description,omitempty"`
	Behavior         string   `json:"behavior"`
	Ref              string   `json:"ref,omitempty"`
	DependsOn        []string `json:"depends_on,omitempty"`
	DependsOnPayload string   `json:"depends_on_payload,omitempty"`
	AutoStart        bool     `json:"auto_start,omitempty"`
}

// uuidPattern matches a standard UUID (8-4-4-4-12 hex digits).
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

// ResolvePayloadTasks parses a JSON array of task specs from payload_patch.tasks,
// pre-assigns UUIDs, resolves ref-based depends_on entries within the batch,
// and returns Task objects ready for creation.
//
// Each returned task has:
//   - A pre-assigned ID (UUID)
//   - ParentID set to parentID
//   - ProjectID set to projectID
//   - DependsOn entries resolved: refs are replaced with the corresponding task's UUID
//
// Returns an error if a ref in depends_on cannot be resolved within the batch.
func ResolvePayloadTasks(parentID, projectID string, tasksJSON json.RawMessage) ([]*Task, error) {
	if len(tasksJSON) == 0 || string(tasksJSON) == "null" || string(tasksJSON) == "[]" {
		return nil, nil
	}

	var specs []payloadTaskSpec
	if err := json.Unmarshal(tasksJSON, &specs); err != nil {
		return nil, fmt.Errorf("parse payload tasks: %w", err)
	}

	if len(specs) == 0 {
		return nil, nil
	}

	// Pre-assign UUIDs and build ref → ID mapping.
	tasks := make([]*Task, len(specs))
	refToID := make(map[string]string, len(specs))

	for i, spec := range specs {
		id := uuid.New().String()
		tasks[i] = &Task{
			ID:               id,
			ProjectID:        projectID,
			ParentID:         parentID,
			Title:            spec.Title,
			Description:      spec.Description,
			Behavior:         spec.Behavior,
			Ref:              spec.Ref,
			DependsOnPayload: spec.DependsOnPayload,
			AutoStart:        spec.AutoStart,
		}
		if spec.Ref != "" {
			refToID[spec.Ref] = id
		}
	}

	// Resolve depends_on: replace refs with UUIDs.
	for i, spec := range specs {
		if len(spec.DependsOn) == 0 {
			continue
		}
		resolved := make([]string, 0, len(spec.DependsOn))
		for _, dep := range spec.DependsOn {
			if isUUID(dep) {
				// Already a UUID, pass through.
				resolved = append(resolved, dep)
				continue
			}
			// Treat as a ref within the batch.
			depID, ok := refToID[dep]
			if !ok {
				return nil, fmt.Errorf("depends_on ref %q not found in batch (task %q)", dep, spec.Title)
			}
			resolved = append(resolved, depID)
		}
		tasks[i].DependsOn = resolved
	}

	return tasks, nil
}
