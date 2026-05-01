package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestIsReadonly(t *testing.T) {
	cases := []struct {
		name     string
		readonly bool
		status   orchestrator.TaskStatus
		want     bool
	}{
		{"task readonly, executing", true, orchestrator.TaskStatusExecuting, true},
		{"task readonly, pending", true, orchestrator.TaskStatusPending, true},
		{"task not readonly, executing", false, orchestrator.TaskStatusExecuting, false},
		{"task not readonly, pending", false, orchestrator.TaskStatusPending, false},
		{"task not readonly, done", false, orchestrator.TaskStatusDone, false},
		{"task not readonly, aborted", false, orchestrator.TaskStatusAborted, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &orchestrator.Task{Readonly: tc.readonly, Status: tc.status}
			got := orchestrator.IsReadonly(task)
			if got != tc.want {
				t.Errorf("IsReadonly(readonly=%v, %q) = %v, want %v",
					tc.readonly, tc.status, got, tc.want)
			}
		})
	}
}
