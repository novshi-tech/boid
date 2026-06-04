package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubGlobalJobStore struct {
	capturedFilter JobListFilter
	jobs           []JobWithContext
	err            error
}

func (s *stubGlobalJobStore) ListJobsWithContext(filter JobListFilter) ([]JobWithContext, error) {
	s.capturedFilter = filter
	return s.jobs, s.err
}

func TestJobHandler_listGlobal_TasklessParam(t *testing.T) {
	store := &stubGlobalJobStore{
		jobs: []JobWithContext{
			{Job: Job{ID: "j1", Status: JobStatusRunning}},
		},
	}
	h := &JobHandler{
		Jobs:   &stubJobStore{},
		Global: store,
	}

	req := httptest.NewRequest("GET", "/api/jobs?status=running&taskless=true", nil)
	w := httptest.NewRecorder()
	h.listGlobal(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if store.capturedFilter.Status != "running" {
		t.Errorf("Status = %q, want running", store.capturedFilter.Status)
	}
	if !store.capturedFilter.TasklessOnly {
		t.Error("TasklessOnly = false, want true")
	}

	var got []JobWithContext
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].ID != "j1" {
		t.Errorf("unexpected response: %+v", got)
	}
}

func TestJobHandler_listGlobal_NoTasklessParam(t *testing.T) {
	store := &stubGlobalJobStore{jobs: []JobWithContext{}}
	h := &JobHandler{
		Jobs:   &stubJobStore{},
		Global: store,
	}

	req := httptest.NewRequest("GET", "/api/jobs?status=running", nil)
	w := httptest.NewRecorder()
	h.listGlobal(w, req)

	if store.capturedFilter.TasklessOnly {
		t.Error("TasklessOnly = true, want false when param absent")
	}
}
