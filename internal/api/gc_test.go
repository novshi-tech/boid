package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type stubGCStore struct {
	result *orchestrator.GCResult
	err    error
}

func (s *stubGCStore) GC(olderThan time.Duration, dryRun bool) (*orchestrator.GCResult, error) {
	return s.result, s.err
}

func TestGCHandler_Run(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		storeResult *orchestrator.GCResult
		storeErr   error
		wantStatus int
		wantTasks  int64
		wantDryRun bool
	}{
		{
			name:        "default older_than",
			body:        `{}`,
			storeResult: &orchestrator.GCResult{Tasks: 3, Jobs: 5, Actions: 8, Worktrees: 1},
			wantStatus:  http.StatusOK,
			wantTasks:   3,
		},
		{
			name:        "custom older_than",
			body:        `{"older_than":"24h"}`,
			storeResult: &orchestrator.GCResult{Tasks: 1},
			wantStatus:  http.StatusOK,
			wantTasks:   1,
		},
		{
			name:        "dry_run",
			body:        `{"dry_run":true}`,
			storeResult: &orchestrator.GCResult{Tasks: 2, Jobs: 3},
			wantStatus:  http.StatusOK,
			wantTasks:   2,
			wantDryRun:  true,
		},
		{
			name:       "invalid older_than",
			body:       `{"older_than":"notaduration"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid json",
			body:       `{bad json`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "store error",
			body:       `{}`,
			storeErr:   fmt.Errorf("db error"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &GCAppService{Store: &stubGCStore{result: tc.storeResult, err: tc.storeErr}}
			h := &GCHandler{Service: svc}
			r := h.Routes()

			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(tc.body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				return
			}

			var resp gcResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Tasks != tc.wantTasks {
				t.Errorf("tasks = %d, want %d", resp.Tasks, tc.wantTasks)
			}
			if resp.DryRun != tc.wantDryRun {
				t.Errorf("dry_run = %v, want %v", resp.DryRun, tc.wantDryRun)
			}
		})
	}
}

func TestGCAppService_DefaultOlderThan(t *testing.T) {
	var capturedOlderThan time.Duration
	store := &captureGCStore{}
	svc := &GCAppService{Store: store}
	h := &GCHandler{Service: svc}
	r := h.Routes()

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	capturedOlderThan = store.lastOlderThan
	if capturedOlderThan != 30*24*time.Hour {
		t.Errorf("default older_than = %v, want %v", capturedOlderThan, 30*24*time.Hour)
	}
}

type captureGCStore struct {
	lastOlderThan time.Duration
	lastDryRun    bool
}

func (s *captureGCStore) GC(olderThan time.Duration, dryRun bool) (*orchestrator.GCResult, error) {
	s.lastOlderThan = olderThan
	s.lastDryRun = dryRun
	return &orchestrator.GCResult{}, nil
}

type stubDeviceGCStore struct {
	n   int64
	err error
}

func (s *stubDeviceGCStore) DeleteRevokedDevices(_ context.Context, _ bool) (int64, error) {
	return s.n, s.err
}

func TestGCAppService_DeviceCleanup(t *testing.T) {
	taskResult := &orchestrator.GCResult{Tasks: 1}
	svc := &GCAppService{
		Store:       &stubGCStore{result: taskResult},
		DeviceStore: &stubDeviceGCStore{n: 3},
	}

	result, err := svc.Run(24*time.Hour, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Tasks != 1 {
		t.Errorf("Tasks = %d, want 1", result.Tasks)
	}
	if result.Devices != 3 {
		t.Errorf("Devices = %d, want 3", result.Devices)
	}
}

func TestGCAppService_NoDeviceStore(t *testing.T) {
	svc := &GCAppService{Store: &stubGCStore{result: &orchestrator.GCResult{Tasks: 2}}}
	result, err := svc.Run(24*time.Hour, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Devices != 0 {
		t.Errorf("Devices = %d, want 0", result.Devices)
	}
}

func TestGCAppService_DeviceError_DoesNotFail(t *testing.T) {
	svc := &GCAppService{
		Store:       &stubGCStore{result: &orchestrator.GCResult{}},
		DeviceStore: &stubDeviceGCStore{err: fmt.Errorf("db error")},
	}
	_, err := svc.Run(24*time.Hour, false)
	if err != nil {
		t.Errorf("Run should not fail on device GC error, got: %v", err)
	}
}
