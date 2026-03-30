package hook_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/hook"
	"github.com/novshi-tech/boid/internal/model"
)

func TestIsReadonly(t *testing.T) {
	cases := []struct {
		name     string
		readonly bool
		status   model.TaskStatus
		want     bool
	}{
		{"behavior readonly, executing", true, model.TaskStatusExecuting, true},
		{"behavior readonly, pending", true, model.TaskStatusPending, true},
		{"behavior not readonly, executing", false, model.TaskStatusExecuting, false},
		{"behavior not readonly, verifying", false, model.TaskStatusVerifying, true},
		{"behavior not readonly, in_review", false, model.TaskStatusInReview, true},
		{"behavior not readonly, pending", false, model.TaskStatusPending, false},
		{"behavior not readonly, collecting_feedback", false, model.TaskStatusCollectingFeedback, false},
		{"behavior not readonly, done", false, model.TaskStatusDone, false},
		{"behavior not readonly, aborted", false, model.TaskStatusAborted, false},
		{"behavior readonly, verifying", true, model.TaskStatusVerifying, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			behavior := &model.TaskBehavior{Readonly: tc.readonly}
			got := hook.IsReadonly(behavior, tc.status)
			if got != tc.want {
				t.Errorf("IsReadonly(readonly=%v, %q) = %v, want %v",
					tc.readonly, tc.status, got, tc.want)
			}
		})
	}
}
