package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
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
