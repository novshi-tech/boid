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

func newTestWebHandlerWithInstructions(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/edit/instructions", h.EditInstructionsList)
	r.Get("/tasks/{id}/edit/instructions/{role}", h.EditInstructionsRole)
	r.Post("/tasks/{id}/edit/instructions/{role}", h.PostEditInstructionsRole)
	return r
}

func TestWebHandler_EditInstructionsList_Renders(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
				Instructions: map[string]orchestrator.Instruction{
					"main": {Type: "execution", Consumer: "claude-code", Message: "do the task"},
				},
			},
		},
	}
	r := newTestWebHandlerWithInstructions(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/instructions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, "main") {
		t.Errorf("should list role 'main', got: %s", body)
	}
	if !strings.Contains(body, "/tasks/task-1/edit/instructions/main") {
		t.Errorf("should link to role 'main', got: %s", body)
	}
}

func TestWebHandler_EditInstructionsList_EmptyInstructions(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
			},
		},
	}
	r := newTestWebHandlerWithInstructions(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/instructions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "instructionsなし") {
		t.Errorf("empty instructions should show 'instructionsなし', got: %s", body)
	}
}

func TestWebHandler_EditInstructionsRole_Renders(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
				Instructions: map[string]orchestrator.Instruction{
					"main": {
						Type:     "execution",
						Consumer: "claude-code",
						Message:  "do the task",
						Model:    "sonnet",
					},
				},
			},
		},
	}
	r := newTestWebHandlerWithInstructions(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/instructions/main", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, `name="consumer"`) {
		t.Error("form should contain consumer field")
	}
	if !strings.Contains(body, "claude-code") {
		t.Errorf("form should contain current consumer value, got: %s", body)
	}
	if !strings.Contains(body, "do the task") {
		t.Errorf("form should contain current message value, got: %s", body)
	}
	if !strings.Contains(body, "sonnet") {
		t.Errorf("form should contain current model value, got: %s", body)
	}
}

func TestWebHandler_EditInstructionsRole_NewRole(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
			},
		},
	}
	r := newTestWebHandlerWithInstructions(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/instructions/newrole", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `name="consumer"`) {
		t.Error("form should contain consumer field for new role")
	}
}

func TestWebHandler_PostEditInstructionsRole_Success(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
			},
		},
	}
	r := newTestWebHandlerWithInstructions(svc)

	body := url.Values{
		"type":     {"execution"},
		"consumer": {"claude-code"},
		"message":  {"do something"},
		"model":    {"sonnet"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/instructions/main", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1/edit/instructions" {
		t.Errorf("Location = %q, want /tasks/task-1/edit/instructions", loc)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	var patch map[string]orchestrator.Instruction
	if err := json.Unmarshal(svc.updateTaskCalls[0].Instructions, &patch); err != nil {
		t.Fatalf("Instructions is not valid JSON: %v", err)
	}
	inst, ok := patch["main"]
	if !ok {
		t.Fatal("patch should contain 'main' role")
	}
	if inst.Consumer != "claude-code" {
		t.Errorf("consumer = %q, want claude-code", inst.Consumer)
	}
	if inst.Type != "execution" {
		t.Errorf("type = %q, want execution", inst.Type)
	}
}

func TestWebHandler_PostEditInstructionsRole_NewRole(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
			},
		},
	}
	r := newTestWebHandlerWithInstructions(svc)

	body := url.Values{
		"type":     {"rework"},
		"consumer": {"claude-code"},
		"message":  {"fix the issues"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/instructions/fixer", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	var patch map[string]orchestrator.Instruction
	if err := json.Unmarshal(svc.updateTaskCalls[0].Instructions, &patch); err != nil {
		t.Fatalf("Instructions is not valid JSON: %v", err)
	}
	if _, ok := patch["fixer"]; !ok {
		t.Error("patch should contain 'fixer' role")
	}
}

func TestWebHandler_PostEditInstructionsRole_InvalidPayload(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
			},
		},
	}
	r := newTestWebHandlerWithInstructions(svc)

	// consumer 空 → バリデーションエラー
	body := url.Values{
		"type":     {"execution"},
		"consumer": {""},
		"message":  {"do something"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/instructions/main", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "consumer") {
		t.Errorf("response should mention consumer validation error, got: %s", respBody)
	}
	if !strings.Contains(respBody, `name="consumer"`) {
		t.Error("form should be re-rendered with consumer field")
	}
	if len(svc.updateTaskCalls) != 0 {
		t.Error("UpdateTask should not be called on invalid input")
	}
}
