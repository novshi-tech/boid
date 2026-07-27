package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		name        string
		body        string
		storeResult *orchestrator.GCResult
		storeErr    error
		wantStatus  int
		wantTasks   int64
		wantDryRun  bool
	}{
		{
			name:        "default older_than",
			body:        `{}`,
			storeResult: &orchestrator.GCResult{Tasks: 3, Jobs: 5, Actions: 8},
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

// --- workspace_homes listing (docs/plans/home-workspace-volume.md Phase 4
// PR5, rewired onto the engine's volume API by 論点 a-2 / PR7 of
// docs/plans/workspace-home-volume-persistence.md) ---

func TestGCHandler_Run_NoHomeStore_OmitsWorkspaceHomes(t *testing.T) {
	svc := &GCAppService{Store: &stubGCStore{result: &orchestrator.GCResult{Tasks: 1}}}
	h := &GCHandler{Service: svc} // Homes left nil.
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp gcResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WorkspaceHomes != nil {
		t.Errorf("WorkspaceHomes = %+v, want nil (no engine handle wired)", resp.WorkspaceHomes)
	}
}

func TestGCHandler_Run_WithHomeStore_ListsWorkspaceHomesWithOrphanFlag(t *testing.T) {
	store := newStubHomeStore(map[string]int64{
		"default":   100,
		"known-ws":  200,
		"orphan-ws": 50,
	})
	svc := &GCAppService{Store: &stubGCStore{result: &orchestrator.GCResult{}}}
	h := &GCHandler{
		Service: svc,
		Homes:   store,
		Workspaces: &stubWorkspaceSlugLister{summaries: []*orchestrator.WorkspaceSummary{
			{ID: "default"}, {ID: "known-ws"},
		}},
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp gcResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.WorkspaceHomes) != 3 {
		t.Fatalf("len(WorkspaceHomes) = %d, want 3: %+v", len(resp.WorkspaceHomes), resp.WorkspaceHomes)
	}
	byslug := map[string]WorkspaceHomeSize{}
	for _, e := range resp.WorkspaceHomes {
		byslug[e.Slug] = e
	}
	if byslug["default"].Orphan {
		t.Error("default: Orphan = true, want false")
	}
	if byslug["known-ws"].Bytes != 200 {
		t.Errorf("known-ws: Bytes = %d, want 200", byslug["known-ws"].Bytes)
	}
	if want := store.volumeName("known-ws"); byslug["known-ws"].Volume != want {
		t.Errorf("known-ws: Volume = %q, want %q", byslug["known-ws"].Volume, want)
	}
	if !byslug["orphan-ws"].Orphan {
		t.Error("orphan-ws: Orphan = false, want true")
	}
	if resp.WorkspaceHomesListError != "" {
		t.Errorf("WorkspaceHomesListError = %q, want empty (lister succeeded)", resp.WorkspaceHomesListError)
	}
}

// TestGCHandler_Run_WithHomeStore_ListerError_ReportsListErrorAndEmptyHomes
// pins Should-fix #3 (codex PR #791 review) at the /api/gc response level: a
// lister failure must not come back as every home mismarked Orphan=true —
// WorkspaceHomes is reported empty and WorkspaceHomesListError carries the
// reason (selection A, see ListWorkspaceHomeSizes's doc comment).
func TestGCHandler_Run_WithHomeStore_ListerError_ReportsListErrorAndEmptyHomes(t *testing.T) {
	svc := &GCAppService{Store: &stubGCStore{result: &orchestrator.GCResult{}}}
	h := &GCHandler{
		Service:    svc,
		Homes:      newStubHomeStore(map[string]int64{"known-ws": 1}),
		Workspaces: &stubWorkspaceSlugLister{err: fmt.Errorf("db unavailable")},
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp gcResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.WorkspaceHomes) != 0 {
		t.Errorf("len(WorkspaceHomes) = %d, want 0 (omitted on lister failure): %+v", len(resp.WorkspaceHomes), resp.WorkspaceHomes)
	}
	if resp.WorkspaceHomesListError == "" {
		t.Error("WorkspaceHomesListError = empty, want the lister's error message")
	}
}

// TestGCHandler_Run_DryRun_StillListsWorkspaceHomes pins that --dry-run does
// not suppress the listing: it is visibility-only reporting, not a pending
// mutation, so there is nothing for --dry-run to gate. Moved here from cmd's
// TestRunGC_DryRun_StillReportsWorkspaceHomes, which could no longer observe
// a listing once it became engine-backed (PR7).
func TestGCHandler_Run_DryRun_StillListsWorkspaceHomes(t *testing.T) {
	svc := &GCAppService{Store: &stubGCStore{result: &orchestrator.GCResult{}}}
	h := &GCHandler{Service: svc, Homes: newStubHomeStore(map[string]int64{"known-ws": 500})}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"dry_run":true}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp gcResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.DryRun {
		t.Error("DryRun = false, want true")
	}
	if len(resp.WorkspaceHomes) != 1 {
		t.Fatalf("len(WorkspaceHomes) = %d, want 1 (--dry-run must not suppress a read-only listing): %+v", len(resp.WorkspaceHomes), resp.WorkspaceHomes)
	}
}

// TestGCHandler_Run_WithHomeStore_EnumerationFailure_ReportsWhyAndKeepsTheRest
// pins both halves of what an engine that cannot list volumes must cost the
// caller.
//
// The cheap half: GC's own record-deletion counts are already computed and
// must still be reported.
//
// The half this test exists for (PR7 round-2 codex review, Major 2): the
// REASON has to reach the response. Logging it daemon-side and sending back an
// omitted section makes "the engine is down" indistinguishable from "this
// install has no workspace home volumes" — both are an absent workspace_homes
// key and an empty workspace_homes_list_error — so `boid gc` printed nothing
// at all and an operator had no way to tell a broken engine from a clean one.
// The host-path implementation this replaced did set listErr on its own
// enumeration failure, so going quiet here was a regression, not a new gap.
func TestGCHandler_Run_WithHomeStore_EnumerationFailure_ReportsWhyAndKeepsTheRest(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"known-ws": 1})
	store.listErr = fmt.Errorf("engine down")
	svc := &GCAppService{Store: &stubGCStore{result: &orchestrator.GCResult{Tasks: 3}}}
	h := &GCHandler{Service: svc, Homes: store}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp gcResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Tasks != 3 {
		t.Errorf("Tasks = %d, want 3 (GC's own work must survive a volume-listing failure)", resp.Tasks)
	}
	if len(resp.WorkspaceHomes) != 0 {
		t.Errorf("WorkspaceHomes = %+v, want empty", resp.WorkspaceHomes)
	}
	if !strings.Contains(resp.WorkspaceHomesListError, "engine down") {
		t.Errorf("WorkspaceHomesListError = %q, want it to carry the engine's failure — otherwise a down engine "+
			"is indistinguishable from an install with no workspace home volumes", resp.WorkspaceHomesListError)
	}
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
