package templates

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func renderSessionList(t *testing.T, sessions []SessionView, projectFilter string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := SessionList(sessions, projectFilter).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func newSessionView(id, projectName, handlerID string) SessionView {
	return SessionView{
		ID:          id,
		ProjectName: projectName,
		HandlerID:   handlerID,
		DisplayName: "",
		CreatedAt:   time.Now(),
	}
}

func TestSessionList_LinksToJobDetail(t *testing.T) {
	sessions := []SessionView{
		newSessionView("job-abc123", "myproject", "claude-code"),
	}
	html := renderSessionList(t, sessions, "")
	want := `/jobs/job-abc123`
	if !strings.Contains(html, want) {
		t.Errorf("session row should link to /jobs/{id}, want %q in: %s", want, html)
	}
}

func TestSessionList_EmptyState(t *testing.T) {
	html := renderSessionList(t, []SessionView{}, "")
	if !strings.Contains(html, "empty-state") {
		t.Error("empty session list should render empty-state")
	}
	if strings.Contains(html, "/terminal") {
		t.Error("empty session list must not contain any terminal links")
	}
}

func TestSessionList_ShowsProjectName(t *testing.T) {
	sessions := []SessionView{
		newSessionView("job-1", "acme-project", "bash"),
	}
	html := renderSessionList(t, sessions, "")
	if !strings.Contains(html, "acme-project") {
		t.Error("session row should display project name")
	}
}

func TestSessionList_ShowsHandlerID(t *testing.T) {
	sessions := []SessionView{
		newSessionView("job-1", "proj", "my-command"),
	}
	html := renderSessionList(t, sessions, "")
	if !strings.Contains(html, "my-command") {
		t.Error("session row should display handler ID when DisplayName is empty")
	}
}

func TestSessionList_PrefersDisplayName(t *testing.T) {
	s := newSessionView("job-1", "proj", "raw-handler")
	s.DisplayName = "Pretty Name"
	html := renderSessionList(t, []SessionView{s}, "")
	if !strings.Contains(html, "Pretty Name") {
		t.Error("session row should prefer DisplayName over HandlerID")
	}
}

func TestSessionList_MultipleRows(t *testing.T) {
	sessions := []SessionView{
		newSessionView("job-1", "proj-a", "cmd-a"),
		newSessionView("job-2", "proj-b", "cmd-b"),
	}
	html := renderSessionList(t, sessions, "")
	if !strings.Contains(html, "/jobs/job-1") {
		t.Error("first session should appear")
	}
	if !strings.Contains(html, "/jobs/job-2") {
		t.Error("second session should appear")
	}
}

func TestSessionList_HasCreateButton(t *testing.T) {
	html := renderSessionList(t, []SessionView{}, "")
	if !strings.Contains(html, "/sessions/new") {
		t.Error("session list should contain a link to /sessions/new")
	}
}

func TestSessionList_EmptyStateLinksToNew(t *testing.T) {
	html := renderSessionList(t, []SessionView{}, "")
	if !strings.Contains(html, "/sessions/new") {
		t.Error("empty state should link to /sessions/new")
	}
}

// SessionNew template tests

func renderSessionNew(t *testing.T, projects []*orchestrator.Project, selectedProjectID string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := SessionNew(projects, selectedProjectID, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestSessionNew_ShowsProjects(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
		{ID: "proj-2", Meta: orchestrator.ProjectMeta{Name: "Other Project"}},
	}
	html := renderSessionNew(t, projects, "")
	if !strings.Contains(html, "My Project") {
		t.Error("should show project name")
	}
	if !strings.Contains(html, "proj-1") {
		t.Error("should include project id as option value")
	}
}

func TestSessionNew_NoProjectSelected_NoStartForm(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
	}
	html := renderSessionNew(t, projects, "")
	if strings.Contains(html, "/sessions/start") {
		t.Error("without project selected, no start-session form should be present")
	}
}

func TestSessionNew_WithProjectShowsHarnessForm(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
	}
	html := renderSessionNew(t, projects, "proj-1")
	want := `/projects/proj-1/sessions/start`
	if !strings.Contains(html, want) {
		t.Errorf("harness form action should target %q, got: %s", want, html)
	}
	for _, harness := range []string{"claude", "codex", "opencode"} {
		if !strings.Contains(html, `value="`+harness+`"`) {
			t.Errorf("harness dropdown missing option %q", harness)
		}
	}
}

func TestSessionList_ElapsedHasDataSince(t *testing.T) {
	now := time.Now()
	s := newSessionView("job-1", "proj", "cmd")
	s.CreatedAt = now
	html := renderSessionList(t, []SessionView{s}, "")
	want := fmt.Sprintf(`data-since="%d"`, now.Unix())
	if !strings.Contains(html, want) {
		t.Errorf("elapsed span should have data-since with unix seconds, want %q in: %s", want, html)
	}
	if !strings.Contains(html, "session-elapsed") {
		t.Error("elapsed span should have class session-elapsed")
	}
}

func TestSessionList_ElapsedZeroCreatedAt(t *testing.T) {
	s := SessionView{ID: "job-zero", ProjectName: "proj", HandlerID: "cmd"}
	// CreatedAt is zero value
	html := renderSessionList(t, []SessionView{s}, "")
	if strings.Contains(html, `data-since="0"`) {
		t.Error("zero CreatedAt should not emit data-since with unix 0")
	}
}

func TestSessionNew_SelectedProjectIsPreselected(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "First"}},
		{ID: "proj-2", Meta: orchestrator.ProjectMeta{Name: "Second"}},
	}
	html := renderSessionNew(t, projects, "proj-2")
	if !strings.Contains(html, `value="proj-2" selected`) {
		t.Errorf("proj-2 should be selected, got html containing proj-2 option: %s",
			html[strings.Index(html, "proj-2"):strings.Index(html, "proj-2")+100])
	}
}

func TestSessionNew_NoInstructionTextarea(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
	}
	html := renderSessionNew(t, projects, "proj-1")
	if strings.Contains(html, `name="instruction"`) {
		t.Error("SessionNew should not render an instruction textarea")
	}
	// Harness select, readonly checkbox, session name, and start button must still be present.
	if !strings.Contains(html, `name="harness_type"`) {
		t.Error("SessionNew should still render the harness select")
	}
	if !strings.Contains(html, `name="readonly"`) {
		t.Error("SessionNew should still render the readonly checkbox")
	}
	if !strings.Contains(html, `name="name"`) {
		t.Error("SessionNew should still render the session name input")
	}
	if !strings.Contains(html, "Start session") {
		t.Error("SessionNew should still render the Start session button")
	}
}
