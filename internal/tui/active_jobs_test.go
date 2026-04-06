package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
)

func TestRenderJobLine_StatusDot(t *testing.T) {
	cases := []struct {
		status  api.JobStatus
		wantDot string
	}{
		{api.JobStatusRunning, "●"},
		{api.JobStatusCompleted, "✓"},
		{api.JobStatusFailed, "✗"},
		{"pending", "○"},
	}
	for _, tc := range cases {
		job := api.JobWithContext{Job: api.Job{ID: "abc12345", Status: tc.status, CreatedAt: time.Now()}}
		line := renderJobLine(job, false, 80)
		if !strings.Contains(line, tc.wantDot) {
			t.Errorf("status %q: want dot %q in %q", tc.status, tc.wantDot, line)
		}
	}
}
