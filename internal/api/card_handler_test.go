package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type stubTriageReader struct {
	view    *CardView
	list    []*CardView
	gotID   string
	gotFilt orchestrator.TaskFilter
	err     error
}

func (s *stubTriageReader) GetCard(taskID string) (*CardView, error) {
	s.gotID = taskID
	if s.err != nil {
		return nil, s.err
	}
	return s.view, nil
}

func (s *stubTriageReader) ListCards(filter orchestrator.TaskFilter) ([]*CardView, error) {
	s.gotFilt = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.list, nil
}

// TestTriageHandler_Routes pins that the collection and single-task routes
// don't shadow each other. GET /api/triage and GET /api/triage/{id} sit at the
// same level, so a mis-registered route would silently send one to the other's
// handler — the listing would answer with a single object, or an id would be
// swallowed as a filter.
func TestTriageHandler_Routes(t *testing.T) {
	svc := &stubTriageReader{
		view: &CardView{TaskID: "t1", Status: orchestrator.TaskStatusParked, ParkedFrom: orchestrator.TaskStatusReady},
		list: []*CardView{{TaskID: "a"}, {TaskID: "b"}},
	}
	h := &CardHandler{Service: svc}

	t.Run("single", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t1", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
		}
		if svc.gotID != "t1" {
			t.Fatalf("service saw id %q, want t1", svc.gotID)
		}
		var got CardView
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body is not a single view (%v): %s", err, rec.Body)
		}
		if got.ParkedFrom != orchestrator.TaskStatusReady {
			t.Fatalf("parked_from = %q, want it serialized through", got.ParkedFrom)
		}
	})

	t.Run("list with filters", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?project_id=meta&status=queue_next", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
		}
		if svc.gotFilt.ProjectID != "meta" || svc.gotFilt.Status != "queue_next" {
			t.Fatalf("filter = %+v, want project_id/status carried through", svc.gotFilt)
		}
		var got []*CardView
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body is not a list (%v): %s", err, rec.Body)
		}
		if len(got) != 2 {
			t.Fatalf("got %d views, want 2", len(got))
		}
	})
}

// TestTriageHandler_PropagatesStatusError pins that a service StatusError keeps
// its code (a missing task must not surface as 200 or 500).
func TestTriageHandler_PropagatesStatusError(t *testing.T) {
	h := &CardHandler{Service: &stubTriageReader{err: &StatusError{Code: http.StatusNotFound, Message: "nope"}}}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}
