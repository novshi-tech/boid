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

type fakeNotifier struct {
	calledTaskID   string
	calledMessage  string
	calledAsk      string
	calledQID      string
	err            error
}

func (n *fakeNotifier) NotifyTask(ctx context.Context, taskID, message, ask, questionID, sessionID string) error {
	n.calledTaskID = taskID
	n.calledMessage = message
	n.calledAsk = ask
	n.calledQID = questionID
	return n.err
}

func notifyRequest(t *testing.T, handler http.Handler, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/"+id+"/notify", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestNotifyHandler_Success(t *testing.T) {
	notifier := &fakeNotifier{}
	h := &TaskHandler{Notifier: notifier}

	w := notifyRequest(t, h.Routes(), "task-1", NotifyTaskRequest{Message: "hello"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d (body=%s)", w.Code, http.StatusNoContent, w.Body.String())
	}
	if notifier.calledTaskID != "task-1" || notifier.calledMessage != "hello" {
		t.Errorf("notifier got task=%q message=%q", notifier.calledTaskID, notifier.calledMessage)
	}
	if notifier.calledAsk != "" || notifier.calledQID != "" {
		t.Errorf("ask/questionID should be empty for normal notify, got ask=%q qid=%q", notifier.calledAsk, notifier.calledQID)
	}
}

func TestNotifyHandler_AskMode(t *testing.T) {
	notifier := &fakeNotifier{}
	h := &TaskHandler{Notifier: notifier}

	w := notifyRequest(t, h.Routes(), "task-1", NotifyTaskRequest{Message: "check in", Ask: "Approve?", QuestionID: "q-123"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d (body=%s)", w.Code, http.StatusNoContent, w.Body.String())
	}
	if notifier.calledAsk != "Approve?" {
		t.Errorf("ask = %q, want %q", notifier.calledAsk, "Approve?")
	}
	if notifier.calledQID != "q-123" {
		t.Errorf("question_id = %q, want %q", notifier.calledQID, "q-123")
	}
}

func TestNotifyHandler_ServiceError(t *testing.T) {
	notifier := &fakeNotifier{err: &StatusError{Code: http.StatusNotImplemented, Message: "not configured"}}
	h := &TaskHandler{Notifier: notifier}

	w := notifyRequest(t, h.Routes(), "task-1", NotifyTaskRequest{Message: "hello"})
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestNotifyHandler_GenericError(t *testing.T) {
	notifier := &fakeNotifier{err: errors.New("boom")}
	h := &TaskHandler{Notifier: notifier}

	w := notifyRequest(t, h.Routes(), "task-1", NotifyTaskRequest{Message: "hello"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestNotifyHandler_NoNotifier(t *testing.T) {
	// Notifier=nil → route is not registered, so 405 (Method Not Allowed) or 404
	h := &TaskHandler{}
	w := notifyRequest(t, h.Routes(), "task-1", NotifyTaskRequest{Message: "hello"})
	if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 or 405", w.Code)
	}
}

func TestNotifyHandler_BadJSON(t *testing.T) {
	notifier := &fakeNotifier{}
	h := &TaskHandler{Notifier: notifier}

	req := httptest.NewRequest(http.MethodPost, "/task-1/notify", bytes.NewReader([]byte("not-json")))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "task-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusBadRequest)
	}
	if notifier.calledTaskID != "" {
		t.Error("notifier should not have been called on bad JSON")
	}
}
