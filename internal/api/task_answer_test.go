package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

type fakeAnswerer struct {
	calledTaskID   string
	calledQID      string
	calledAnswer   string
	err            error
}

func (a *fakeAnswerer) AnswerTask(ctx context.Context, taskID, questionID, answer string) error {
	a.calledTaskID = taskID
	a.calledQID = questionID
	a.calledAnswer = answer
	return a.err
}

func answerRequest(t *testing.T, handler http.Handler, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/"+id+"/answer", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestAnswerHandler_Success(t *testing.T) {
	answerer := &fakeAnswerer{}
	h := &TaskHandler{Answerer: answerer}

	w := answerRequest(t, h.Routes(), "task-1", AnswerTaskRequest{QuestionID: "q-123", Answer: "yes"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d (body=%s)", w.Code, http.StatusNoContent, w.Body.String())
	}
	if answerer.calledTaskID != "task-1" {
		t.Errorf("task id = %q, want task-1", answerer.calledTaskID)
	}
	if answerer.calledQID != "q-123" {
		t.Errorf("question_id = %q, want q-123", answerer.calledQID)
	}
	if answerer.calledAnswer != "yes" {
		t.Errorf("answer = %q, want yes", answerer.calledAnswer)
	}
}

func TestAnswerHandler_ServiceError(t *testing.T) {
	answerer := &fakeAnswerer{err: &StatusError{Code: http.StatusConflict, Message: "task is not awaiting"}}
	h := &TaskHandler{Answerer: answerer}

	w := answerRequest(t, h.Routes(), "task-1", AnswerTaskRequest{QuestionID: "q-1", Answer: "yes"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestAnswerHandler_GenericError(t *testing.T) {
	answerer := &fakeAnswerer{err: errors.New("db error")}
	h := &TaskHandler{Answerer: answerer}

	w := answerRequest(t, h.Routes(), "task-1", AnswerTaskRequest{QuestionID: "q-1", Answer: "yes"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestAnswerHandler_NoAnswerer(t *testing.T) {
	h := &TaskHandler{}
	w := answerRequest(t, h.Routes(), "task-1", AnswerTaskRequest{QuestionID: "q-1", Answer: "yes"})
	if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 or 405", w.Code)
	}
}

func TestAnswerHandler_BadJSON(t *testing.T) {
	answerer := &fakeAnswerer{}
	h := &TaskHandler{Answerer: answerer}

	req := httptest.NewRequest(http.MethodPost, "/task-1/answer", bytes.NewReader([]byte("not-json")))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "task-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusBadRequest)
	}
	if answerer.calledTaskID != "" {
		t.Error("answerer should not have been called on bad JSON")
	}
}
