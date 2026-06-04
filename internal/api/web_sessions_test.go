package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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
	if !strings.Contains(body, "/jobs/job-abc/terminal") {
		t.Error("should link to /jobs/{id}/terminal")
	}
	if !strings.Contains(body, "my-project") {
		t.Error("should show project name")
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
	if !strings.Contains(body, "/jobs/job-1/terminal") {
		t.Error("project-a job should appear")
	}
	if strings.Contains(body, "/jobs/job-2/terminal") {
		t.Error("project-b job should be filtered out")
	}
}
