package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newTestWebHandlerWithDeps(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}/edit/deps", h.EditDeps)
	r.Post("/tasks/{id}/edit/deps", h.PostEditDeps)
	return r
}

func TestWebHandler_EditDeps_Renders(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:               "task-1",
				Title:            "My Task",
				DependsOn:        []string{"dep-1", "dep-2"},
				DependsOnPayload: "artifact.merged",
				ParentID:         "parent-1",
			},
			DependsOnResolved: []*orchestrator.Task{
				{ID: "dep-1", Title: "Dep Task 1", Status: "done"},
				{ID: "dep-2", Title: "Dep Task 2", Status: "pending"},
			},
		},
	}
	r := newTestWebHandlerWithDeps(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/deps", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, "dep-1") {
		t.Errorf("should show dep-1 in current deps list, got: %s", body)
	}
	if !strings.Contains(body, "dep-2") {
		t.Errorf("should show dep-2 in current deps list, got: %s", body)
	}
	if !strings.Contains(body, `name="depends_on"`) {
		t.Error("form should contain depends_on textarea")
	}
	if !strings.Contains(body, `name="depends_on_payload"`) {
		t.Error("form should contain depends_on_payload input")
	}
	if !strings.Contains(body, `name="parent_id"`) {
		t.Error("form should contain parent_id input")
	}
	if !strings.Contains(body, "artifact.merged") {
		t.Errorf("form should contain current depends_on_payload value, got: %s", body)
	}
	if !strings.Contains(body, "parent-1") {
		t.Errorf("form should contain current parent_id value, got: %s", body)
	}
}

func TestWebHandler_PostEditDeps_Add(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:        "task-1",
				Title:     "My Task",
				DependsOn: []string{},
			},
		},
	}
	r := newTestWebHandlerWithDeps(svc)

	body := url.Values{
		"depends_on":        {"new-dep-id"},
		"depends_on_payload": {""},
		"parent_id":         {""},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/deps", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	call := svc.updateTaskCalls[0]
	if len(call.DependsOn) != 1 || call.DependsOn[0] != "new-dep-id" {
		t.Errorf("DependsOn = %v, want [new-dep-id]", call.DependsOn)
	}
}

func TestWebHandler_PostEditDeps_Remove(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:        "task-1",
				Title:     "My Task",
				DependsOn: []string{"dep-1"},
			},
		},
	}
	r := newTestWebHandlerWithDeps(svc)

	body := url.Values{
		"depends_on":        {""},
		"depends_on_payload": {""},
		"parent_id":         {""},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/deps", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	call := svc.updateTaskCalls[0]
	if len(call.DependsOn) != 0 {
		t.Errorf("DependsOn = %v, want empty (remove all)", call.DependsOn)
	}
}

func TestWebHandler_PostEditDeps_ServiceError_Returns400(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
			},
		},
		updateTaskErr: &StatusError{Code: http.StatusBadRequest, Message: `depends_on: task "bad-id" not found`},
	}
	r := newTestWebHandlerWithDeps(svc)

	body := url.Values{
		"depends_on":        {"bad-id"},
		"depends_on_payload": {""},
		"parent_id":         {""},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/deps", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "not found") {
		t.Errorf("response should contain error message, got: %s", respBody)
	}
	if !strings.Contains(respBody, `name="depends_on"`) {
		t.Error("form should be re-rendered on error")
	}
}

func TestWebHandler_PostEditDeps_PayloadAndParentID(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:    "task-1",
				Title: "My Task",
			},
		},
	}
	r := newTestWebHandlerWithDeps(svc)

	body := url.Values{
		"depends_on":        {""},
		"depends_on_payload": {"artifact.auto-merge.merged"},
		"parent_id":         {"parent-xyz"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/deps", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	call := svc.updateTaskCalls[0]
	if call.DependsOnPayload == nil || *call.DependsOnPayload != "artifact.auto-merge.merged" {
		t.Errorf("DependsOnPayload = %v, want artifact.auto-merge.merged", call.DependsOnPayload)
	}
	if call.ParentID == nil || *call.ParentID != "parent-xyz" {
		t.Errorf("ParentID = %v, want parent-xyz", call.ParentID)
	}
}

// --- TaskAppService.UpdateTask の depends_on 拡張テスト ---

func TestTaskAppServiceUpdateTask_DependsOn_Add(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep-1", Status: orchestrator.TaskStatusPending}
	task := &orchestrator.Task{ID: "task-1", Title: "my task", Status: orchestrator.TaskStatusPending, DependsOn: nil}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		"dep-1":  dep,
		"task-1": task,
	}}
	svc := &TaskAppService{Tasks: store}

	updated, err := svc.UpdateTask("task-1", UpdateTaskRequest{DependsOn: []string{"dep-1"}})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if len(updated.DependsOn) != 1 || updated.DependsOn[0] != "dep-1" {
		t.Errorf("DependsOn = %v, want [dep-1]", updated.DependsOn)
	}
}

func TestTaskAppServiceUpdateTask_DependsOn_Remove(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Title: "my task", Status: orchestrator.TaskStatusPending, DependsOn: []string{"dep-1"}}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{"task-1": task}}
	svc := &TaskAppService{Tasks: store}

	updated, err := svc.UpdateTask("task-1", UpdateTaskRequest{DependsOn: []string{}})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if len(updated.DependsOn) != 0 {
		t.Errorf("DependsOn = %v, want empty", updated.DependsOn)
	}
}

func TestTaskAppServiceUpdateTask_DependsOn_InvalidID(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Title: "my task", Status: orchestrator.TaskStatusPending}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{"task-1": task}}
	svc := &TaskAppService{Tasks: store}

	_, err := svc.UpdateTask("task-1", UpdateTaskRequest{DependsOn: []string{"nonexistent-id"}})
	if err == nil {
		t.Fatal("UpdateTask() error = nil, want error for nonexistent depends_on ID")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %v", err)
	}
}

func TestTaskAppServiceUpdateTask_DependsOn_Cycle(t *testing.T) {
	taskA := &orchestrator.Task{ID: "task-a", Title: "A", Status: orchestrator.TaskStatusPending, DependsOn: []string{"task-b"}}
	taskB := &orchestrator.Task{ID: "task-b", Title: "B", Status: orchestrator.TaskStatusPending}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		"task-a": taskA,
		"task-b": taskB,
	}}
	svc := &TaskAppService{Tasks: store}

	// task-b が task-a に依存しようとすると循環依存 (task-a → task-b → task-a)
	_, err := svc.UpdateTask("task-b", UpdateTaskRequest{DependsOn: []string{"task-a"}})
	if err == nil {
		t.Fatal("UpdateTask() error = nil, want error for circular dependency")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %v", err)
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error message should mention 'circular', got: %s", err.Error())
	}
}

func TestTaskAppServiceUpdateTask_DependsOnPayload(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Title: "my task", Status: orchestrator.TaskStatusPending}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{"task-1": task}}
	svc := &TaskAppService{Tasks: store}

	payload := "artifact.merged"
	updated, err := svc.UpdateTask("task-1", UpdateTaskRequest{DependsOnPayload: &payload})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if updated.DependsOnPayload != "artifact.merged" {
		t.Errorf("DependsOnPayload = %q, want artifact.merged", updated.DependsOnPayload)
	}
}

func TestTaskAppServiceUpdateTask_ParentID(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Title: "my task", Status: orchestrator.TaskStatusPending}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{"task-1": task}}
	svc := &TaskAppService{Tasks: store}

	parentID := "parent-xyz"
	updated, err := svc.UpdateTask("task-1", UpdateTaskRequest{ParentID: &parentID})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if updated.ParentID != "parent-xyz" {
		t.Errorf("ParentID = %q, want parent-xyz", updated.ParentID)
	}
}

// PATCH /tasks/{id} に depends_on を渡した場合のテスト
func TestTaskHandlerPatch_DependsOnly(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Title: "original", DependsOn: nil}
	svc := &patchTaskService{task: task}
	h := &TaskHandler{Service: svc}

	w := patchRequest(t, http.HandlerFunc(h.Patch), "t1", map[string]any{
		"depends_on": []string{"dep-abc"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}
