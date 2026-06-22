package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestPostAnswer_MultipartSavesAttachments confirms the multipart form path
// persists files to <attachmentsRoot>/tasks/<id>/attachments before invoking
// AnswerTask. The legacy url-encoded behaviour is exercised by the existing
// TestWebHandler_PostAnswer_* tests; this one is the new code path.
func TestPostAnswer_MultipartSavesAttachments(t *testing.T) {
	attachRoot := t.TempDir()
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusAwaiting
	svc := &stubAnswerService{
		stubWebService: stubWebService{taskDetail: detail},
	}
	h := &WebHandler{Service: svc, AttachmentsRoot: attachRoot}
	r := chi.NewRouter()
	r.Post("/tasks/{id}/answer", h.PostAnswer)

	body, contentType := makeMultipartAnswerBody(t, "qid-mp", "here is the shot", "shot.png", "image/png", []byte("PNGDATA"))
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/answer", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body=%q", w.Code, w.Body.String())
	}
	if len(svc.answerCalls) != 1 {
		t.Fatalf("AnswerTask calls = %d, want 1", len(svc.answerCalls))
	}
	if svc.answerCalls[0].answer != "here is the shot" {
		t.Errorf("answer = %q, want 'here is the shot'", svc.answerCalls[0].answer)
	}
	saved, err := os.ReadFile(filepath.Join(attachRoot, "tasks", "task-1", "attachments", "shot.png"))
	if err != nil {
		t.Fatalf("attachment not persisted: %v", err)
	}
	if string(saved) != "PNGDATA" {
		t.Errorf("attachment body = %q, want PNGDATA", string(saved))
	}
}

// TestPostAnswer_LegacyURLEncodedStillWorks confirms parseTaskForm correctly
// dispatches to ParseForm() when no multipart body is present (regression
// guard for the existing form-urlencoded clients).
func TestPostAnswer_LegacyURLEncodedStillWorks(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusAwaiting
	svc := &stubAnswerService{
		stubWebService: stubWebService{taskDetail: detail},
	}
	// Note: AttachmentsRoot intentionally empty — legacy path must not need it.
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Post("/tasks/{id}/answer", h.PostAnswer)

	body := strings.NewReader("question_id=qid-legacy&answer=plain+answer")
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/answer", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if len(svc.answerCalls) != 1 || svc.answerCalls[0].answer != "plain answer" {
		t.Errorf("answer = %v, want one call with 'plain answer'", svc.answerCalls)
	}
}

// TestPostAnswer_MultipartRejectsBadAttachment verifies validation surfaces
// the error before AnswerTask is called (the agent shouldn't see a half-
// processed turn just because the upload was malformed).
func TestPostAnswer_MultipartRejectsBadAttachment(t *testing.T) {
	attachRoot := t.TempDir()
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusAwaiting
	svc := &stubAnswerService{
		stubWebService: stubWebService{taskDetail: detail},
	}
	h := &WebHandler{Service: svc, AttachmentsRoot: attachRoot}
	r := chi.NewRouter()
	r.Post("/tasks/{id}/answer", h.PostAnswer)

	// .exe is outside the allowlist; ValidateAttachmentHeaders should bail.
	body, contentType := makeMultipartAnswerBody(t, "qid-bad", "msg", "malware.exe", "application/octet-stream", []byte("MZ\x00\x00"))
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/answer", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirect with ?error=)", w.Code)
	}
	if !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Errorf("Location = %q, expected ?error=", w.Header().Get("Location"))
	}
	if len(svc.answerCalls) != 0 {
		t.Errorf("AnswerTask must not be called when attachment validation fails (got %d calls)", len(svc.answerCalls))
	}
}

// TestPostTaskCreate_MultipartFlow round-trips a multipart submission through
// the full PostTaskCreate handler and confirms the attachment lands in the
// per-task directory keyed off the *created* task ID.
func TestPostTaskCreate_MultipartFlow(t *testing.T) {
	attachRoot := t.TempDir()
	created := &orchestrator.Task{ID: "new-task-1", ProjectID: "proj-1", Title: "demo"}
	svc := &stubWebService{createTaskResult: created}
	h := &WebHandler{Service: svc, AttachmentsRoot: attachRoot}
	r := chi.NewRouter()
	r.Post("/tasks", h.PostTaskCreate)

	body, contentType := makeMultipartCreateBody(t, "demo task", "see screenshot", "ui.png", "image/png", []byte("PNGDATA"))
	req := httptest.NewRequest(http.MethodPost, "/tasks", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body=%q", w.Code, w.Body.String())
	}
	if len(svc.createTaskCalls) != 1 {
		t.Fatalf("CreateTask calls = %d, want 1", len(svc.createTaskCalls))
	}
	saved, err := os.ReadFile(filepath.Join(attachRoot, "tasks", "new-task-1", "attachments", "ui.png"))
	if err != nil {
		t.Fatalf("attachment not persisted to created task dir: %v", err)
	}
	if string(saved) != "PNGDATA" {
		t.Errorf("attachment body = %q, want PNGDATA", string(saved))
	}
}

func makeMultipartAnswerBody(t *testing.T, questionID, answer, fileName, contentType string, body []byte) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if err := mw.WriteField("question_id", questionID); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("answer", answer); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("attachments", fileName)
	if err != nil {
		t.Fatal(err)
	}
	_ = contentType // multipart.CreateFormFile sets application/octet-stream; ValidateAttachmentHeaders only checks filename extension.
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf, mw.FormDataContentType()
}

func makeMultipartCreateBody(t *testing.T, title, description, fileName, contentType string, body []byte) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	for k, v := range map[string]string{
		"title":       title,
		"project_id":  "proj-1",
		"description": description,
		"behavior":    "dev",
	} {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("attachments", fileName)
	if err != nil {
		t.Fatal(err)
	}
	_ = contentType
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf, mw.FormDataContentType()
}

