package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/project"
)

func TestIsReadonly(t *testing.T) {
	cases := []struct {
		name     string
		readonly bool
		status   orchestrator.TaskStatus
		want     bool
	}{
		{"behavior readonly, executing", true, orchestrator.TaskStatusExecuting, true},
		{"behavior readonly, pending", true, orchestrator.TaskStatusPending, true},
		{"behavior not readonly, executing", false, orchestrator.TaskStatusExecuting, false},
		{"behavior not readonly, verifying", false, orchestrator.TaskStatusVerifying, true},
		{"behavior not readonly, in_review", false, orchestrator.TaskStatusInReview, true},
		{"behavior not readonly, pending", false, orchestrator.TaskStatusPending, false},
		{"behavior not readonly, collecting_feedback", false, orchestrator.TaskStatusCollectingFeedback, false},
		{"behavior not readonly, done", false, orchestrator.TaskStatusDone, false},
		{"behavior not readonly, aborted", false, orchestrator.TaskStatusAborted, false},
		{"behavior readonly, verifying", true, orchestrator.TaskStatusVerifying, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			behavior := &project.TaskBehavior{Readonly: tc.readonly}
			got := orchestrator.IsReadonly(behavior, tc.status)
			if got != tc.want {
				t.Errorf("IsReadonly(readonly=%v, %q) = %v, want %v",
					tc.readonly, tc.status, got, tc.want)
			}
		})
	}
}
