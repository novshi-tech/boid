package api

import "github.com/novshi-tech/boid/internal/orchestrator"

// isCardTask reports whether t is a card.
func isCardTask(t *orchestrator.Task) bool {
	return t != nil && t.Type == orchestrator.TaskTypeCard
}

// fanOutChildEventToParentCard broadcasts ev to child's parent, but only
// when the parent is a card. A resolution failure is a silent no-op.
func fanOutChildEventToParentCard(hub *TaskEventHub, tasks TaskStore, child *orchestrator.Task, ev TaskEvent) {
	if hub == nil || tasks == nil || child == nil || child.ParentID == "" {
		return
	}
	parent, err := tasks.GetTask(child.ParentID)
	if err != nil || !isCardTask(parent) {
		return
	}
	hub.Broadcast(parent.ID, ev)
}
