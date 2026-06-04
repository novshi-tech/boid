package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubWebServiceWithSessions extends stubWebService with a configurable ListSessions.
type stubWebServiceWithSessions struct {
	stubWebService
	sessions    []JobWithContext
	sessionsErr error
}

func (s *stubWebServiceWithSessions) ListSessions() ([]JobWithContext, error) {
	return s.sessions, s.sessionsErr
}

func newTestWebHandlerSessions(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/sessions", h.SessionList)
	return r
}

func TestSessionList_Handler_Empty(t *testing.T) {
	svc := &stubWebServiceWithSessions{}
	r := newTestWebHandlerSessions(svc)

	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "empty-state") {
		t.Error("empty sessions should render empty-state")
	}
}

func TestSessionList_Handler_ShowsJob(t *testing.T) {
	svc := &stubWebServiceWithSessions{
		sessions: []JobWithContext{
			{
				Job: Job{
					ID:        "job-abc",
					HandlerID: "test-cmd",
					Status:    JobStatusRunning,
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				},
				ProjectName: "my-project",
			},
		},
	}
	r := newTestWebHandlerSessions(svc)

	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/jobs/job-abc") {
		t.Error("should link to /jobs/{id}")
	}
	if !strings.Contains(body, "my-project") {
		t.Error("should show project name")
	}
}

func newTestWebHandlerSessionNew(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/sessions/new", h.SessionNew)
	return r
}

func TestSessionNew_Handler_RendersProjects(t *testing.T) {
	svc := &stubWebService{
		projects: []*orchestrator.Project{
			{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
		},
	}
	r := newTestWebHandlerSessionNew(svc)

	req := httptest.NewRequest(http.MethodGet, "/sessions/new", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, "My Project") {
		t.Error("should list project name")
	}
	if !strings.Contains(body, "proj-1") {
		t.Error("should include project id as option value")
	}
}

func TestSessionNew_Handler_WithProjectShowsCommands(t *testing.T) {
	svc := &stubWebService{
		projects: []*orchestrator.Project{
			{ID: "proj-1", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
		},
		projectCommands: []CommandSummary{
			{Name: "build", Command: []string{"make", "build"}},
			{Name: "test", Command: []string{"go", "test", "./..."}, Readonly: true},
		},
	}
	r := newTestWebHandlerSessionNew(svc)

	req := httptest.NewRequest(http.MethodGet, "/sessions/new?project=proj-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "build") {
		t.Error("should show command name 'build'")
	}
	if !strings.Contains(body, "make build") {
		t.Error("should show command preview")
	}
}

func TestSessionNew_Handler_CommandFormAction(t *testing.T) {
	svc := &stubWebService{
		projects: []*orchestrator.Project{
			{ID: "proj-abc", Meta: orchestrator.ProjectMeta{Name: "My Project"}},
		},
		projectCommands: []CommandSummary{
			{Name: "deploy", Command: []string{"./deploy.sh"}},
		},
	}
	r := newTestWebHandlerSessionNew(svc)

	req := httptest.NewRequest(http.MethodGet, "/sessions/new?project=proj-abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	want := `/projects/proj-abc/commands/deploy/execute`
	if !strings.Contains(body, want) {
		t.Errorf("form action should be %q, got: %s", want, body[:min(500, len(body))])
	}
}

func TestSessionNew_RouteRegistered(t *testing.T) {
	svc := &stubWebService{}
	h := &WebHandler{Service: svc}
	r := h.Routes()

	req := httptest.NewRequest(http.MethodGet, "/sessions/new", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "404 page not found") {
		t.Error("/sessions/new route should be registered in WebHandler.Routes()")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("/sessions/new status = %d, want 200", w.Code)
	}
}

func TestSessionList_Handler_ProjectFilter(t *testing.T) {
	svc := &stubWebServiceWithSessions{
		sessions: []JobWithContext{
			{
				Job: Job{
					ID:        "job-1",
					ProjectID: "proj-a",
					Status:    JobStatusRunning,
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				},
				ProjectName: "Project A",
			},
			{
				Job: Job{
					ID:        "job-2",
					ProjectID: "proj-b",
					Status:    JobStatusRunning,
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				},
				ProjectName: "Project B",
			},
		},
	}
	r := newTestWebHandlerSessions(svc)

	req := httptest.NewRequest(http.MethodGet, "/sessions?project=proj-a", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/jobs/job-1") {
		t.Error("project-a job should appear")
	}
	if strings.Contains(body, "/jobs/job-2") {
		t.Error("project-b job should be filtered out")
	}
}
