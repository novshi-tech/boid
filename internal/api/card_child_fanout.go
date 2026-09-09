package api

import "github.com/novshi-tech/boid/internal/orchestrator"

func isCardTask(t *orchestrator.Task) bool {
	return t != nil && t.Type == orchestrator.TaskTypeCard
}

// fanOutChildEventToParent broadcasts ev to a child's direct parent so its
// detail page can refresh the child row. A resolution failure is a no-op.
func fanOutChildEventToParent(hub *TaskEventHub, tasks TaskStore, child *orchestrator.Task, ev TaskEvent) {
	if hub == nil || tasks == nil || child == nil || child.ParentID == "" {
		return
	}
	parent, err := tasks.GetTask(child.ParentID)
	if err != nil || parent == nil {
		return
	}
	hub.Broadcast(parent.ID, ev)
}

// childEventPayload builds a "child" TaskEvent's payload: child_task_id,
// reason, and one caller-chosen id field.
func childEventPayload(childTaskID, reason, idKey, idValue string) map[string]any {
	return map[string]any{
		"child_task_id": childTaskID,
		"reason":        reason,
		idKey:           idValue,
	}
}
