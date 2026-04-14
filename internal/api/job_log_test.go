package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func newChiContext(params map[string]string) context.Context {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return context.WithValue(context.Background(), chi.RouteCtxKey, rctx)
}

type stubJobLogReader struct {
	data []byte
	err  error
}

func (r *stubJobLogReader) ReadJobLog(runtimeID string) ([]byte, error) {
	return r.data, r.err
}

func TestJobHandler_Log_OK(t *testing.T) {
	job := &Job{
		ID:        "job-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		RuntimeID: "runtime-1",
		Status:    JobStatusCompleted,
	}
	h := &JobHandler{
		Jobs:      &stubJobStore{job: job},
		LogReader: &stubJobLogReader{data: []byte("log content\n")},
	}

	req := httptest.NewRequest("GET", "/job-1/log", nil)
	w := httptest.NewRecorder()

	// call Log directly with the chi param set via chi context
	rctx := newChiContext(map[string]string{"id": "job-1"})
	h.Log(w, req.WithContext(rctx))

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := w.Body.String()
	if body != "log content\n" {
		t.Errorf("body = %q, want %q", body, "log content\n")
	}
}

func TestJobHandler_Log_JobNotFound(t *testing.T) {
	h := &JobHandler{
		Jobs:      &stubJobStore{},
		LogReader: &stubJobLogReader{},
	}

	req := httptest.NewRequest("GET", "/missing/log", nil)
	w := httptest.NewRecorder()
	rctx := newChiContext(map[string]string{"id": "missing"})
	h.Log(w, req.WithContext(rctx))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestJobHandler_Log_NoRuntimeID(t *testing.T) {
	job := &Job{
		ID:        "job-2",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		RuntimeID: "", // no runtime
		Status:    JobStatusCompleted,
	}
	h := &JobHandler{
		Jobs:      &stubJobStore{job: job},
		LogReader: &stubJobLogReader{},
	}

	req := httptest.NewRequest("GET", "/job-2/log", nil)
	w := httptest.NewRecorder()
	rctx := newChiContext(map[string]string{"id": "job-2"})
	h.Log(w, req.WithContext(rctx))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "log not available") {
		t.Errorf("body %q should contain 'log not available'", w.Body.String())
	}
}

func TestJobHandler_Log_RuntimeGCed(t *testing.T) {
	job := &Job{
		ID:        "job-3",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		RuntimeID: "runtime-gone",
		Status:    JobStatusCompleted,
	}
	h := &JobHandler{
		Jobs:      &stubJobStore{job: job},
		LogReader: &stubJobLogReader{err: os.ErrNotExist},
	}

	req := httptest.NewRequest("GET", "/job-3/log", nil)
	w := httptest.NewRecorder()
	rctx := newChiContext(map[string]string{"id": "job-3"})
	h.Log(w, req.WithContext(rctx))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "log not available") {
		t.Errorf("body %q should contain 'log not available'", w.Body.String())
	}
}

func TestJobHandler_Log_ReadError(t *testing.T) {
	job := &Job{
		ID:        "job-4",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		RuntimeID: "runtime-err",
		Status:    JobStatusRunning,
	}
	h := &JobHandler{
		Jobs:      &stubJobStore{job: job},
		LogReader: &stubJobLogReader{err: errors.New("disk error")},
	}

	req := httptest.NewRequest("GET", "/job-4/log", nil)
	w := httptest.NewRecorder()
	rctx := newChiContext(map[string]string{"id": "job-4"})
	h.Log(w, req.WithContext(rctx))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
