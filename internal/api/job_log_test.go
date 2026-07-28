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

// TestJobHandler_Log_JobNotFound pins that an id that names no job at all
// says so — the single most common way to reach this endpoint by mistake is
// passing a TASK id (`boid job log <task-id>`), and the pre-fix body claimed
// "runtime cleaned up", which describes a completely different situation (a
// job that really ran, whose transcript was later removed). A 2026-07-28
// dogfood session lost time to exactly that misreport.
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
	body := w.Body.String()
	if !strings.Contains(body, "no such job") {
		t.Errorf("body %q should say the job does not exist", body)
	}
	if !strings.Contains(body, "boid job list --task") {
		t.Errorf("body %q should point at the task-id mixup", body)
	}
	if strings.Contains(body, "cleaned up") {
		t.Errorf("body %q must not claim a runtime was cleaned up: no job ever existed", body)
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
	body := w.Body.String()
	if !strings.Contains(body, "log not available") {
		t.Errorf("body %q should contain 'log not available'", body)
	}
	if !strings.Contains(body, "no runtime") {
		t.Errorf("body %q should say the job has no runtime yet", body)
	}
	if !strings.Contains(body, string(JobStatusCompleted)) {
		t.Errorf("body %q should carry the job status, which is what tells a reader whether the sandbox is still coming up or already gave up", body)
	}
	if strings.Contains(body, "cleaned up") {
		t.Errorf("body %q must not claim a runtime was cleaned up: none was ever assigned", body)
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
	body := w.Body.String()
	if !strings.Contains(body, "log not available") {
		t.Errorf("body %q should contain 'log not available'", body)
	}
	if !strings.Contains(body, "runtime-gone") {
		t.Errorf("body %q should name the runtime whose transcript is missing, so the reader can go look for it by hand", body)
	}
}

// TestJobHandler_Log_404BodiesAreDistinct is the mutation guard for the three
// tests above: each one on its own would still pass if the handler collapsed
// back to a single shared "log not available (runtime cleaned up)" string for
// every 404, as long as that string happened to contain the substrings each
// test looks for. The whole point of the fix is that a reader can tell the
// three situations apart, so assert they are pairwise different.
func TestJobHandler_Log_404BodiesAreDistinct(t *testing.T) {
	logBody := func(h *JobHandler, id string) string {
		req := httptest.NewRequest("GET", "/"+id+"/log", nil)
		w := httptest.NewRecorder()
		h.Log(w, req.WithContext(newChiContext(map[string]string{"id": id})))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 for %s", w.Code, id)
		}
		return w.Body.String()
	}

	noJob := logBody(&JobHandler{
		Jobs:      &stubJobStore{},
		LogReader: &stubJobLogReader{},
	}, "missing")
	noRuntime := logBody(&JobHandler{
		Jobs:      &stubJobStore{job: &Job{ID: "job-2", RuntimeID: "", Status: JobStatusRunning}},
		LogReader: &stubJobLogReader{},
	}, "job-2")
	gone := logBody(&JobHandler{
		Jobs:      &stubJobStore{job: &Job{ID: "job-3", RuntimeID: "runtime-gone", Status: JobStatusCompleted}},
		LogReader: &stubJobLogReader{err: os.ErrNotExist},
	}, "job-3")

	for _, pair := range []struct {
		a, b  string
		aName string
		bName string
	}{
		{noJob, noRuntime, "job-not-found", "no-runtime"},
		{noJob, gone, "job-not-found", "transcript-gone"},
		{noRuntime, gone, "no-runtime", "transcript-gone"},
	} {
		if pair.a == pair.b {
			t.Errorf("%s and %s produce the same body %q; they are different situations and must read differently", pair.aName, pair.bName, pair.a)
		}
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
