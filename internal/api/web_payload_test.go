package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newTestWebHandlerWithPayload(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/edit/payload", h.EditPayloadList)
	r.Get("/tasks/{id}/edit/payload/{section}", h.EditPayloadSection)
	r.Post("/tasks/{id}/edit/payload/{section}", h.PostEditPayloadSection)
	return r
}

func TestWebHandler_EditPayloadList_Renders(t *testing.T) {
	payload := json.RawMessage(`{"alpha":{"key":"val"},"beta":42}`)
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:      "task-1",
				Title:   "My Task",
				Status:  "pending",
				Payload: payload,
			},
		},
	}
	r := newTestWebHandlerWithPayload(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/payload", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, "alpha") {
		t.Errorf("should list top-level key 'alpha', got: %s", body)
	}
	if !strings.Contains(body, "beta") {
		t.Errorf("should list top-level key 'beta', got: %s", body)
	}
	if !strings.Contains(body, "/tasks/task-1/edit/payload/alpha") {
		t.Errorf("should link to section 'alpha', got: %s", body)
	}
}

func TestWebHandler_EditPayloadList_EmptyPayload(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:     "task-1",
				Title:  "My Task",
				Status: "pending",
			},
		},
	}
	r := newTestWebHandlerWithPayload(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/payload", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "セクションなし") {
		t.Errorf("empty payload should show セクションなし, got: %s", body)
	}
}

func TestWebHandler_EditPayloadSection_Renders(t *testing.T) {
	payload := json.RawMessage(`{"mykey":{"foo":"bar","num":1}}`)
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:      "task-1",
				Title:   "My Task",
				Status:  "pending",
				Payload: payload,
			},
		},
	}
	r := newTestWebHandlerWithPayload(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/payload/mykey", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, `name="yaml_text"`) {
		t.Error("form should contain yaml_text textarea")
	}
	if !strings.Contains(body, "foo") {
		t.Errorf("textarea should contain YAML content with 'foo', got: %s", body)
	}
}

func TestWebHandler_EditPayloadSection_NonExistent(t *testing.T) {
	payload := json.RawMessage(`{"existing":"value"}`)
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:      "task-1",
				Title:   "My Task",
				Status:  "pending",
				Payload: payload,
			},
		},
	}
	r := newTestWebHandlerWithPayload(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/payload/newkey", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `name="yaml_text"`) {
		t.Error("form should contain yaml_text textarea even for non-existent section")
	}
}

func TestWebHandler_PostEditPayloadSection_Success(t *testing.T) {
	payload := json.RawMessage(`{"existing":"value"}`)
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:      "task-1",
				Title:   "My Task",
				Status:  "pending",
				Payload: payload,
			},
		},
	}
	r := newTestWebHandlerWithPayload(svc)

	yamlInput := "key: value\nnum: 42\n"
	body := url.Values{"yaml_text": {yamlInput}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/payload/mysection", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1/edit/payload" {
		t.Errorf("Location = %q, want /tasks/task-1/edit/payload", loc)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(svc.updateTaskCalls[0].Payload, &merged); err != nil {
		t.Fatalf("merged payload is invalid JSON: %v", err)
	}
	if _, ok := merged["existing"]; !ok {
		t.Error("merged payload should preserve existing section")
	}
	if _, ok := merged["mysection"]; !ok {
		t.Error("merged payload should contain new section 'mysection'")
	}
}

func TestWebHandler_PostEditPayloadSection_InvalidYAML(t *testing.T) {
	payload := json.RawMessage(`{"existing":"value"}`)
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:      "task-1",
				Title:   "My Task",
				Status:  "pending",
				Payload: payload,
			},
		},
	}
	r := newTestWebHandlerWithPayload(svc)

	badYAML := "key: [\ninvalid yaml"
	body := url.Values{"yaml_text": {badYAML}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/payload/mysection", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "YAML parse error") {
		t.Errorf("response should contain YAML parse error, got: %s", respBody)
	}
	if !strings.Contains(respBody, `name="yaml_text"`) {
		t.Error("form should be re-rendered with textarea")
	}
	if len(svc.updateTaskCalls) != 0 {
		t.Error("UpdateTask should not be called on invalid YAML")
	}
}

func TestWebHandler_PostEditPayloadSection_ArrayPayload(t *testing.T) {
	// top-level が JSON array のタスクは壊さない (TUI と同方針)
	payload := json.RawMessage(`[1,2,3]`)
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:      "task-1",
				Title:   "My Task",
				Status:  "pending",
				Payload: payload,
			},
		},
	}
	r := newTestWebHandlerWithPayload(svc)

	yamlInput := "foo: bar\n"
	body := url.Values{"yaml_text": {yamlInput}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/payload/mysection", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// mergeSection が array を無視して新しいオブジェクトを作る (フォールバック)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
}
