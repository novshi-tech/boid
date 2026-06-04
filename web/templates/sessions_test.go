package templates

import (
	"bytes"
	"context"
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

func TestSessionList_LinksToTerminal(t *testing.T) {
	sessions := []SessionView{
		newSessionView("job-abc123", "myproject", "claude-code"),
	}
	html := renderSessionList(t, sessions, "")
	want := `/jobs/job-abc123/terminal`
	if !strings.Contains(html, want) {
		t.Errorf("session row should link to /jobs/{id}/terminal, want %q in: %s", want, html)
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
	if !strings.Contains(html, "/jobs/job-1/terminal") {
		t.Error("first session should appear")
	}
	if !strings.Contains(html, "/jobs/job-2/terminal") {
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

func renderSessionNew(t *testing.T, projects []*orchestrator.Project, selectedProjectID string, commands []CommandView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := SessionNew(projects, selectedProjectID, commands, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestSessionNew_ShowsProjects(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
		{ID: "proj-2", Meta: orchestrator.ProjectMeta{Name: "Other Project"}},
	}
	html := renderSessionNew(t, projects, "", nil)
	if !strings.Contains(html, "My Project") {
		t.Error("should show project name")
	}
	if !strings.Contains(html, "proj-1") {
		t.Error("should include project id as option value")
	}
}

func TestSessionNew_NoProjectSelected_NoCommands(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
	}
	html := renderSessionNew(t, projects, "", nil)
	if strings.Contains(html, "/execute") {
		t.Error("without project selected, no execute form actions should be present")
	}
}

func TestSessionNew_WithProjectShowsCommands(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
	}
	commands := []CommandView{
		{Name: "build", Command: []string{"make", "build"}},
		{Name: "test", Command: []string{"go", "test", "./..."}, Readonly: true},
	}
	html := renderSessionNew(t, projects, "proj-1", commands)
	if !strings.Contains(html, "build") {
		t.Error("should show command name")
	}
	if !strings.Contains(html, "make build") {
		t.Error("should show command preview")
	}
	if !strings.Contains(html, "readonly") {
		t.Error("should show readonly badge")
	}
}

func TestSessionNew_CommandFormAction(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-abc", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
	}
	commands := []CommandView{
		{Name: "deploy", Command: []string{"./deploy.sh"}},
	}
	html := renderSessionNew(t, projects, "proj-abc", commands)
	want := `/projects/proj-abc/commands/deploy/execute`
	if !strings.Contains(html, want) {
		t.Errorf("form action should be %q, html: %s", want, html)
	}
}

func TestSessionNew_EmptyCommands(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
	}
	html := renderSessionNew(t, projects, "proj-1", []CommandView{})
	if !strings.Contains(html, "empty-state") {
		t.Error("should show empty state when project has no commands")
	}
	if strings.Contains(html, "/execute") {
		t.Error("empty commands should not show any execute forms")
	}
}

func TestSessionNew_SelectedProjectIsPreselected(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "First"}},
		{ID: "proj-2", Meta: orchestrator.ProjectMeta{Name: "Second"}},
	}
	html := renderSessionNew(t, projects, "proj-2", nil)
	if !strings.Contains(html, `value="proj-2" selected`) {
		t.Errorf("proj-2 should be selected, got html containing proj-2 option: %s",
			html[strings.Index(html, "proj-2"):strings.Index(html, "proj-2")+100])
	}
}
