package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func jobPatchRequest(t *testing.T, handler http.Handler, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/"+id, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestJobHandlerPatch_UpdatesDisplayName(t *testing.T) {
	store := &stubJobStore{job: &Job{ID: "j1", DisplayName: "old"}}
	h := &JobHandler{Jobs: store}

	w := jobPatchRequest(t, http.HandlerFunc(h.Patch), "j1", map[string]any{
		"display_name": "new name",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if store.job.DisplayName != "new name" {
		t.Errorf("DisplayName = %q, want %q", store.job.DisplayName, "new name")
	}
}

func TestJobHandlerPatch_TrimsWhitespace(t *testing.T) {
	store := &stubJobStore{job: &Job{ID: "j1", DisplayName: "old"}}
	h := &JobHandler{Jobs: store}

	w := jobPatchRequest(t, http.HandlerFunc(h.Patch), "j1", map[string]any{
		"display_name": "  trimmed  ",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if store.job.DisplayName != "trimmed" {
		t.Errorf("DisplayName = %q, want %q", store.job.DisplayName, "trimmed")
	}
}

func TestJobHandlerPatch_AllowsEmptyDisplayName(t *testing.T) {
	store := &stubJobStore{job: &Job{ID: "j1", DisplayName: "old"}}
	h := &JobHandler{Jobs: store}

	w := jobPatchRequest(t, http.HandlerFunc(h.Patch), "j1", map[string]any{
		"display_name": "",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if store.job.DisplayName != "" {
		t.Errorf("DisplayName = %q, want empty", store.job.DisplayName)
	}
}

func TestJobHandlerPatch_MissingDisplayName_ReturnsBadRequest(t *testing.T) {
	store := &stubJobStore{job: &Job{ID: "j1"}}
	h := &JobHandler{Jobs: store}

	w := jobPatchRequest(t, http.HandlerFunc(h.Patch), "j1", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestJobHandlerPatch_JobNotFound_ReturnsNotFound(t *testing.T) {
	store := &stubJobStore{job: &Job{ID: "other"}}
	h := &JobHandler{Jobs: store}

	w := jobPatchRequest(t, http.HandlerFunc(h.Patch), "j-missing", map[string]any{
		"display_name": "name",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}
