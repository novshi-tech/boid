package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	data     []byte
	err      error
	statSize int64
	statTime time.Time
	statErr  error
}

func (r *stubJobLogReader) ReadJobLog(runtimeID string) ([]byte, error) {
	return r.data, r.err
}

func (r *stubJobLogReader) StatJobLog(runtimeID string) (int64, time.Time, error) {
	return r.statSize, r.statTime, r.statErr
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

func TestJobHandler_Get_WithTranscriptStat(t *testing.T) {
	past := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	job := &Job{
		ID:        "job-ts",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		RuntimeID: "runtime-ts",
		Status:    JobStatusRunning,
	}
	h := &JobHandler{
		Jobs: &stubJobStore{job: job},
		LogReader: &stubJobLogReader{
			statSize: 1234,
			statTime: past,
		},
	}

	req := httptest.NewRequest("GET", "/job-ts", nil)
	w := httptest.NewRecorder()
	rctx := newChiContext(map[string]string{"id": "job-ts"})
	h.Get(w, req.WithContext(rctx))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"transcript_size":1234`) {
		t.Errorf("body missing transcript_size: %s", body)
	}
	if !strings.Contains(body, `"transcript_mtime"`) {
		t.Errorf("body missing transcript_mtime: %s", body)
	}
	if !strings.Contains(body, `"transcript_idle_seconds"`) {
		t.Errorf("body missing transcript_idle_seconds: %s", body)
	}
}

func TestJobHandler_Get_NoRuntime(t *testing.T) {
	job := &Job{
		ID:        "job-nr",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		RuntimeID: "",
		Status:    JobStatusCompleted,
	}
	h := &JobHandler{
		Jobs:      &stubJobStore{job: job},
		LogReader: &stubJobLogReader{statSize: 999},
	}

	req := httptest.NewRequest("GET", "/job-nr", nil)
	w := httptest.NewRecorder()
	rctx := newChiContext(map[string]string{"id": "job-nr"})
	h.Get(w, req.WithContext(rctx))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "transcript_size") {
		t.Errorf("body should not contain transcript_size when RuntimeID is empty: %s", body)
	}
}

func TestJobHandler_Get_TranscriptMissing(t *testing.T) {
	job := &Job{
		ID:        "job-tm",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		RuntimeID: "runtime-tm",
		Status:    JobStatusCompleted,
	}
	h := &JobHandler{
		Jobs:      &stubJobStore{job: job},
		LogReader: &stubJobLogReader{statErr: os.ErrNotExist},
	}

	req := httptest.NewRequest("GET", "/job-tm", nil)
	w := httptest.NewRecorder()
	rctx := newChiContext(map[string]string{"id": "job-tm"})
	h.Get(w, req.WithContext(rctx))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when transcript is missing", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "transcript_size") {
		t.Errorf("body should not contain transcript_size when file is missing: %s", body)
	}
}
