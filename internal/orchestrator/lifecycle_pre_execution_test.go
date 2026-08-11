package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// IsInstructionsEditable / IsPreDispatchEditableStatus は pending に加えて
// captured/triaged/ready でも編集を許す (整形セッション UC-3 のため) が、
// parked は除外する — 後回し中のタスクは編集対象外という意図 (docs/plans/
// cross-project-issue-triage.md Phase 1 PR-1)。
func TestIsInstructionsEditable_PreExecutionStatuses(t *testing.T) {
	editable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusReady,
	}
	notEditable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusDropped,
	}
	for _, s := range editable {
		if !orchestrator.IsInstructionsEditable(s) {
			t.Errorf("IsInstructionsEditable(%q) = false, want true", s)
		}
	}
	for _, s := range notEditable {
		if orchestrator.IsInstructionsEditable(s) {
			t.Errorf("IsInstructionsEditable(%q) = true, want false", s)
		}
	}
}
