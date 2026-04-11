package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
)

// TestActiveJobsQKey_ReturnsPopScreen verifies q returns popScreenMsg (go back to list).
func TestActiveJobsQKey_ReturnsPopScreen(t *testing.T) {
	shared := &SharedState{Panes: make(map[string]string)}
	s := NewActiveJobsScreen(shared)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("q: expected popScreenMsg, got %T", msg)
	}
}

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
