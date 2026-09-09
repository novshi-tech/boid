package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
)

type childResultTimelineStore struct {
	page *timeline.CardTimelinePage
	err  error
}

func (s childResultTimelineStore) BuildCardTimeline(string, string, int) (*timeline.CardTimelinePage, error) {
	return s.page, s.err
}

func (s childResultTimelineStore) CardPinnedItems(string) ([]timeline.CardItem, error) {
	return nil, nil
}

func childDetailHandler(t *testing.T, child orchestrator.TaskTriageChild, execution *TaskDetailView) (*stubWebService, http.Handler) {
	t.Helper()
	detail, err := json.Marshal(map[string]any{"children": []orchestrator.TaskTriageChild{child}})
	if err != nil {
		t.Fatal(err)
	}
	parent := &TaskDetailView{Task: &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, Title: "Parent card"}}
	svc := &stubWebService{taskDetails: map[string]*TaskDetailView{"card-1": parent}}
	if execution != nil {
		svc.taskDetails[execution.Task.ID] = execution
	}
	h := &WebHandler{Service: svc, TaskTriage: &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"card-1": {TaskID: "card-1", Detail: detail},
	}}}
	r := chi.NewRouter()
	r.Get("/tasks/{id}/children/{child_id}", h.TaskChildDetail)
	return svc, r
}

func TestTaskChildDetail_SpecBeforeDispatch(t *testing.T) {
	_, r := childDetailHandler(t, orchestrator.TaskTriageChild{
		ID: "child-1", Title: "Investigate mobile navigation", Status: orchestrator.TaskTriageChildStatusSpecced,
		Spec: &orchestrator.TaskTriageChildSpec{Project: "web", Behavior: "implement", Description: "Keep the title readable.", Instruction: "Add focused tests."},
	}, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/card-1/children/child-1", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for _, want := range []string{"Investigate mobile navigation", "Keep the title readable.", "Add focused tests.", `href="/tasks/card-1"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %q in child spec detail", want)
		}
	}
}

func TestTaskChildDetail_ResolvesExistingExecutionTask(t *testing.T) {
	exec := &TaskDetailView{Task: &orchestrator.Task{ID: "exec-1", Type: orchestrator.TaskTypeExecution, ParentID: "card-1"}}
	_, r := childDetailHandler(t, orchestrator.TaskTriageChild{ID: "child-1", TaskRef: "exec-1"}, exec)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/card-1/children/child-1", nil))

	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/tasks/exec-1" {
		t.Fatalf("response = %d Location=%q", w.Code, w.Header().Get("Location"))
	}
}

func TestTaskChildDetail_TransientExecutionReadFailureIsNotCalledDeleted(t *testing.T) {
	svc, r := childDetailHandler(t, orchestrator.TaskTriageChild{ID: "child-1", TaskRef: "exec-1"}, nil)
	svc.taskDetailErrs = map[string]error{"exec-1": assertTransientError{}}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/card-1/children/child-1", nil))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "no longer available") {
		t.Fatalf("transient read failure misreported; status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTaskChildDetail_DoesNotRedirectToTaskOwnedByAnotherParent(t *testing.T) {
	exec := &TaskDetailView{Task: &orchestrator.Task{ID: "exec-1", Type: orchestrator.TaskTypeExecution, ParentID: "other-card"}}
	_, r := childDetailHandler(t, orchestrator.TaskTriageChild{ID: "child-1", TaskRef: "exec-1"}, exec)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/card-1/children/child-1", nil))
	if w.Code != http.StatusOK || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "no longer available") {
		t.Fatalf("foreign task reference should stay on safe fallback; status=%d location=%q", w.Code, w.Header().Get("Location"))
	}
}

type assertTransientError struct{}

func (assertTransientError) Error() string { return "temporary read failure" }

func TestTaskChildDetail_DeletedExecutionFallsBackToSavedSpec(t *testing.T) {
	_, r := childDetailHandler(t, orchestrator.TaskTriageChild{
		ID: "child-1", Title: "Old child", Status: orchestrator.TaskTriageChildStatusClosed, TaskRef: "gone-1",
		Spec: &orchestrator.TaskTriageChildSpec{Description: "Surviving context"},
	}, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/card-1/children/child-1", nil))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "no longer available") || !strings.Contains(w.Body.String(), "Surviving context") {
		t.Fatalf("deleted task fallback missing; status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTaskChildDetail_UnpreparedChildStillHasDetail(t *testing.T) {
	_, r := childDetailHandler(t, orchestrator.TaskTriageChild{ID: "child-1", Title: "Draft child", Status: orchestrator.TaskTriageChildStatusOpen}, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/card-1/children/child-1", nil))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "not ready yet") {
		t.Fatalf("unprepared child detail missing; status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCardChildResult_SurvivesDeletedExecutionTask(t *testing.T) {
	h := &WebHandler{CardTimeline: childResultTimelineStore{
		page: &timeline.CardTimelinePage{Items: []timeline.CardItem{
			{Kind: timeline.CardItemChild, Child: &timeline.CardChildDetail{ChildID: "child-1", HasResult: true, Result: "Completed safely", ResultStatus: orchestrator.TaskStatusDone}},
		}},
	}}
	result, status, ok, err := h.cardChildResult("card-1", "child-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || result != "Completed safely" || status != orchestrator.TaskStatusDone {
		t.Fatalf("result = (%q, %q, %v)", result, status, ok)
	}
}

func TestTaskChildDetail_ResultReadFailureIsNotRenderedAsNoResult(t *testing.T) {
	detail, err := json.Marshal(map[string]any{"children": []orchestrator.TaskTriageChild{{ID: "child-1", TaskRef: "gone-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	h := &WebHandler{
		Service:      &stubWebService{taskDetails: map[string]*TaskDetailView{"card-1": {Task: &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard}}}},
		TaskTriage:   &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{"card-1": {TaskID: "card-1", Detail: detail}}},
		CardTimeline: childResultTimelineStore{err: assertTransientError{}},
	}
	r := chi.NewRouter()
	r.Get("/tasks/{id}/children/{child_id}", h.TaskChildDetail)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/card-1/children/child-1", nil))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "temporarily unavailable") {
		t.Fatalf("result read failure misreported; status=%d body=%s", w.Code, w.Body.String())
	}
}
