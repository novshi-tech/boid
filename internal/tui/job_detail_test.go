package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
)

func makeTestJob(status api.JobStatus) *api.Job {
	return &api.Job{
		ID:          "job-12345678-full-id",
		TaskID:      "task-1",
		Role:        "main",
		RuntimeID:   "runtime-abc",
		WorkspacePath: "/home/user/worktree",
		Interactive: false,
		TTY:         false,
		Status:      status,
		ExitCode:    0,
		Output:      "line1\nline2\nline3\nline4\nline5",
		CreatedAt:   time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2024, 1, 1, 12, 5, 0, 0, time.UTC),
	}
}

func newTestJobDetailScreen(job *api.Job) *JobDetailScreen {
	shared := &SharedState{
		Panes:       make(map[string]string),
		TmuxEnabled: false,
	}
	return NewJobDetailScreen(shared, job)
}

// TestJobDetailView_Renders verifies the View method renders key fields.
func TestJobDetailView_Renders(t *testing.T) {
	job := makeTestJob(api.JobStatusCompleted)
	s := newTestJobDetailScreen(job)

	view := s.View(120, 40)
	if view == "" {
		t.Fatal("View() returned empty string")
	}

	checks := []struct {
		label string
		substr string
	}{
		{"job ID", "job-12345678-full-id"},
		{"role", "main"},
		{"status", "completed"},
		{"runtime ID", "runtime-abc"},
		{"workspace path", "/home/user/worktree"},
		{"Job Detail header", "Job Detail"},
		{"Output header", "Output"},
		{"output line 1", "line1"},
	}
	for _, c := range checks {
		if !containsStr(view, c.substr) {
			t.Errorf("View: expected %s (%q) in output", c.label, c.substr)
		}
	}
}

// TestJobDetailView_NoOutput verifies "(no output)" is shown when output is empty.
func TestJobDetailView_NoOutput(t *testing.T) {
	job := makeTestJob(api.JobStatusRunning)
	job.Output = ""
	s := newTestJobDetailScreen(job)

	view := s.View(80, 30)
	if !containsStr(view, "no output") {
		t.Error("empty output: expected '(no output)' in view")
	}
}

// TestJobDetailEsc_ReturnsPopScreen verifies esc pops the screen.
func TestJobDetailEsc_ReturnsPopScreen(t *testing.T) {
	s := newTestJobDetailScreen(makeTestJob(api.JobStatusRunning))

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("esc: expected popScreenMsg, got %T", msg)
	}
}

// TestJobDetailQ_ReturnsPopScreen verifies q pops the screen.
func TestJobDetailQ_ReturnsPopScreen(t *testing.T) {
	s := newTestJobDetailScreen(makeTestJob(api.JobStatusCompleted))

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("q: expected popScreenMsg, got %T", msg)
	}
}

// TestJobDetailEnter_NoTmux_SetsStatusMsg verifies enter without tmux shows status message.
func TestJobDetailEnter_NoTmux_SetsStatusMsg(t *testing.T) {
	s := newTestJobDetailScreen(makeTestJob(api.JobStatusRunning))
	s.shared.TmuxEnabled = false

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter without tmux: expected non-nil cmd")
	}
	if s.statusMsg == "" {
		t.Error("enter without tmux: expected statusMsg to be set")
	}
}

// TestJobDetailO_NoTmux_SetsStatusMsg verifies 'o' without tmux shows status message.
func TestJobDetailO_NoTmux_SetsStatusMsg(t *testing.T) {
	s := newTestJobDetailScreen(makeTestJob(api.JobStatusRunning))
	s.shared.TmuxEnabled = false

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if cmd == nil {
		t.Fatal("o without tmux: expected non-nil cmd")
	}
	if s.statusMsg == "" {
		t.Error("o without tmux: expected statusMsg to be set")
	}
}

// TestJobDetailOutputScroll_JK verifies j/k scrolls the output.
func TestJobDetailOutputScroll_JK(t *testing.T) {
	job := makeTestJob(api.JobStatusCompleted)
	// output has 5 lines
	s := newTestJobDetailScreen(job)

	if s.outputScroll != 0 {
		t.Errorf("initial outputScroll: want 0, got %d", s.outputScroll)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.outputScroll != 1 {
		t.Errorf("after j: want outputScroll 1, got %d", s.outputScroll)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.outputScroll != 0 {
		t.Errorf("after k: want outputScroll 0, got %d", s.outputScroll)
	}

	// k at 0 stays at 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.outputScroll != 0 {
		t.Errorf("k at 0: want outputScroll 0, got %d", s.outputScroll)
	}
}

// TestJobDetailOutputScroll_Bounded verifies scroll doesn't exceed line count.
func TestJobDetailOutputScroll_Bounded(t *testing.T) {
	job := makeTestJob(api.JobStatusCompleted)
	// output has 5 lines (line1..line5)
	s := newTestJobDetailScreen(job)
	s.outputScroll = 4 // at last line (0-indexed)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.outputScroll > 4 {
		t.Errorf("scroll should not exceed line count, got %d", s.outputScroll)
	}
}

// TestJobDetailView_OutputScroll verifies that scrolled output shows correct lines.
func TestJobDetailView_OutputScroll(t *testing.T) {
	job := makeTestJob(api.JobStatusCompleted)
	s := newTestJobDetailScreen(job)

	// Scroll to line2 (index 1)
	s.outputScroll = 1
	view := s.View(120, 40)
	if !containsStr(view, "line2") {
		t.Error("scroll=1: expected 'line2' to be visible")
	}
}

// TestJobDetailShortHelp verifies ShortHelp is non-empty and mentions key bindings.
func TestJobDetailShortHelp(t *testing.T) {
	s := newTestJobDetailScreen(makeTestJob(api.JobStatusRunning))
	help := s.ShortHelp()
	if help == "" {
		t.Fatal("ShortHelp: expected non-empty string")
	}
	if !containsStr(help, "esc") && !containsStr(help, "q") {
		t.Error("ShortHelp: expected back key hint")
	}
}

// TestJobDetailView_ExitCodeShown verifies exit code is shown for completed/failed jobs.
func TestJobDetailView_ExitCodeShown(t *testing.T) {
	job := makeTestJob(api.JobStatusFailed)
	job.ExitCode = 1
	s := newTestJobDetailScreen(job)

	view := s.View(120, 40)
	if !containsStr(view, "ExitCode: 1") {
		t.Errorf("failed job: expected 'ExitCode: 1' in view, got %q", view)
	}
}

// TestJobDetailView_InteractiveTTYSeparateLines verifies Interactive and TTY are rendered on separate lines.
func TestJobDetailView_InteractiveTTYSeparateLines(t *testing.T) {
	job := makeTestJob(api.JobStatusCompleted)
	job.Interactive = true
	job.TTY = true
	s := newTestJobDetailScreen(job)

	view := s.View(120, 40)
	lines := strings.Split(view, "\n")

	var interactiveLine, ttyLine string
	for _, l := range lines {
		if strings.Contains(l, "Interactive:") {
			interactiveLine = l
		}
		if strings.Contains(l, "TTY:") {
			ttyLine = l
		}
	}

	if interactiveLine == "" {
		t.Fatal("expected a line containing 'Interactive:'")
	}
	if ttyLine == "" {
		t.Fatal("expected a line containing 'TTY:'")
	}
	if interactiveLine == ttyLine {
		t.Error("Interactive and TTY should be on separate lines, but found same line")
	}
	if strings.Contains(interactiveLine, "TTY:") {
		t.Errorf("Interactive line should not contain 'TTY:': %q", interactiveLine)
	}
	if strings.Contains(ttyLine, "Interactive:") {
		t.Errorf("TTY line should not contain 'Interactive:': %q", ttyLine)
	}
}

// TestJobDetailView_MoreLines verifies "... N more lines" indicator is shown.
func TestJobDetailView_MoreLines(t *testing.T) {
	job := makeTestJob(api.JobStatusCompleted)
	// Build output with many lines
	var sb strings.Builder
	for i := range 50 {
		sb.WriteString(fmt.Sprintf("output-line-%d\n", i))
	}
	job.Output = sb.String()
	s := newTestJobDetailScreen(job)

	view := s.View(80, 20) // small height to force truncation
	if !containsStr(view, "more lines") {
		t.Error("many lines: expected '... N more lines' indicator")
	}
}
