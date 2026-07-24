package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// fakeWorkspaceService is a ProjectService stub focused on the workspace CRUD
// surface (docs/plans/workspace-db-consolidation.md PR4 Step C/D/E/F). Every
// non-workspace method panics — WorkspaceHandler never calls them, so a
// panic here means the handler grew an unexpected new dependency.
type fakeWorkspaceService struct {
	createFn          func(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error)
	getFn             func(slug string) (*WorkspaceDetail, error)
	updateFn          func(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error)
	removeFn          func(slug string) error
	listFn            func() ([]*orchestrator.WorkspaceSummary, error)
	exportFn          func(slug string) ([]byte, string, error)
	importFn          func(slug string, meta *orchestrator.WorkspaceMeta, mode string) (*WorkspaceDetail, error)
	applyFn           func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error)
	exportEnvelopesFn func(slugs []string) ([]byte, error)
}

func (s *fakeWorkspaceService) CreateProject(string) (*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *fakeWorkspaceService) ListProjects(string) ([]*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *fakeWorkspaceService) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	if s.listFn != nil {
		return s.listFn()
	}
	panic("not implemented")
}
func (s *fakeWorkspaceService) GetProject(string) (*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *fakeWorkspaceService) SetProjectWorkspace(string, string) (*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *fakeWorkspaceService) DeleteProject(string) error { panic("not implemented") }
func (s *fakeWorkspaceService) ReloadProjects() (*ProjectReloadResult, error) {
	panic("not implemented")
}
func (s *fakeWorkspaceService) ResolveProjectRef(string) ([]*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *fakeWorkspaceService) CreateWorkspace(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error) {
	return s.createFn(slug, meta)
}
func (s *fakeWorkspaceService) GetWorkspace(slug string) (*WorkspaceDetail, error) {
	return s.getFn(slug)
}
func (s *fakeWorkspaceService) UpdateWorkspace(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error) {
	return s.updateFn(slug, meta, ifMatch, force)
}
func (s *fakeWorkspaceService) RemoveWorkspace(slug string) error {
	return s.removeFn(slug)
}
func (s *fakeWorkspaceService) ExportWorkspace(slug string) ([]byte, string, error) {
	return s.exportFn(slug)
}
func (s *fakeWorkspaceService) ImportWorkspace(slug string, meta *orchestrator.WorkspaceMeta, mode string) (*WorkspaceDetail, error) {
	return s.importFn(slug, meta, mode)
}
func (s *fakeWorkspaceService) ApplyWorkspace(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
	if s.applyFn != nil {
		return s.applyFn(apply, dryRun)
	}
	panic("not implemented")
}
func (s *fakeWorkspaceService) ExportWorkspaceEnvelopes(slugs []string) ([]byte, error) {
	if s.exportEnvelopesFn != nil {
		return s.exportEnvelopesFn(slugs)
	}
	panic("not implemented")
}

func doWorkspaceRequest(handler http.Handler, method, path, contentType string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestWorkspaceHandler_Create_Success(t *testing.T) {
	svc := &fakeWorkspaceService{
		createFn: func(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error) {
			if slug != "team-a" {
				t.Errorf("slug = %q, want team-a", slug)
			}
			if !equalStringSliceForTest(meta.HostCommands, []string{"gh"}) {
				t.Errorf("meta.HostCommands = %v", meta.HostCommands)
			}
			return &WorkspaceDetail{Slug: slug, Meta: meta, Revision: "rev-1"}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("slug: team-a\nhost_commands:\n  - gh\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/", "application/yaml", body, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != `"rev-1"` {
		t.Errorf("ETag = %q, want %q", got, `"rev-1"`)
	}
	var detail WorkspaceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Slug != "team-a" {
		t.Errorf("response slug = %q", detail.Slug)
	}
}

func TestWorkspaceHandler_Create_MissingSlugIs400(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/", "application/yaml", []byte("host_commands: [gh]\n"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Create_BadYAMLIs400(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/", "application/yaml", []byte("slug: team-a\nhostcommands: [gh]\n"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field): %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Create_ConflictPropagates409(t *testing.T) {
	svc := &fakeWorkspaceService{
		createFn: func(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error) {
			return nil, &StatusError{Code: http.StatusConflict, Message: "already exists"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/", "application/yaml", []byte("slug: team-a\n"), nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Create_BodyTooLargeIs400(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	big := []byte("slug: team-a\nenv:\n  FOO: \"" + strings.Repeat("x", 2<<20) + "\"\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/", "application/yaml", big, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body too large): %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Show_Success(t *testing.T) {
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}, Revision: "rev-2", AssignedProjects: []string{"p1"}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != `"rev-2"` {
		t.Errorf("ETag = %q, want %q", got, `"rev-2"`)
	}
}

func TestWorkspaceHandler_Show_NotFound(t *testing.T) {
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: "not found"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/ghost", "", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Update_MissingIfMatchWithoutForceIs428(t *testing.T) {
	svc := &fakeWorkspaceService{
		updateFn: func(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error) {
			if ifMatch != "" || force {
				t.Errorf("expected empty ifMatch and force=false, got ifMatch=%q force=%v", ifMatch, force)
			}
			return nil, &StatusError{Code: http.StatusPreconditionRequired, Message: "If-Match required"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a", "application/yaml", []byte("host_commands: [gh]\n"), nil)
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Update_PassesIfMatchHeaderUnquoted(t *testing.T) {
	var gotIfMatch string
	svc := &fakeWorkspaceService{
		updateFn: func(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error) {
			gotIfMatch = ifMatch
			return &WorkspaceDetail{Slug: slug, Meta: meta, Revision: "rev-2"}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a", "application/yaml",
		[]byte("host_commands: [gh]\n"), map[string]string{"If-Match": `"rev-1"`})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotIfMatch != "rev-1" {
		t.Errorf("gotIfMatch = %q, want rev-1 (unquoted)", gotIfMatch)
	}
}

func TestWorkspaceHandler_Update_ForceQueryParamSkipsIfMatch(t *testing.T) {
	var gotForce bool
	svc := &fakeWorkspaceService{
		updateFn: func(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error) {
			gotForce = force
			return &WorkspaceDetail{Slug: slug, Meta: meta}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a?force=true", "application/yaml", []byte("host_commands: [gh]\n"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !gotForce {
		t.Error("expected force=true to be passed through")
	}
}

func TestWorkspaceHandler_Update_MismatchPropagates412(t *testing.T) {
	svc := &fakeWorkspaceService{
		updateFn: func(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error) {
			return nil, &StatusError{Code: http.StatusPreconditionFailed, Message: "stale"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPut, "/team-a", "application/yaml",
		[]byte("host_commands: [gh]\n"), map[string]string{"If-Match": "rev-stale"})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Remove_Success(t *testing.T) {
	var gotSlug string
	svc := &fakeWorkspaceService{
		removeFn: func(slug string) error {
			gotSlug = slug
			return nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotSlug != "team-a" {
		t.Errorf("gotSlug = %q", gotSlug)
	}
}

func TestWorkspaceHandler_Remove_DefaultRejected400(t *testing.T) {
	svc := &fakeWorkspaceService{
		removeFn: func(slug string) error {
			return &StatusError{Code: http.StatusBadRequest, Message: "reserved"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/default", "", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// --- Home directory size / deletion (docs/plans/home-workspace-volume.md
// Phase 4 PR5) ---

func TestWorkspaceHandler_Show_NoRuntimesDir_OmitsHome(t *testing.T) {
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc} // RuntimesDir left empty.
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var detail WorkspaceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Home != nil {
		t.Errorf("Home = %+v, want nil (no RuntimesDir wired)", detail.Home)
	}
}

func TestWorkspaceHandler_Show_WithRuntimesDir_PopulatesHome(t *testing.T) {
	runtimesDir := filepath.Join(t.TempDir(), "runtimes")
	homePath, err := resolveWorkspaceHomePath(runtimesDir, "team-a")
	if err != nil {
		t.Fatalf("resolveWorkspaceHomePath: %v", err)
	}
	if err := os.MkdirAll(homePath, 0o755); err != nil {
		t.Fatalf("mkdir home dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "a.txt"), make([]byte, 42), 0o644); err != nil {
		t.Fatalf("write home file: %v", err)
	}

	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc, RuntimesDir: runtimesDir}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var detail WorkspaceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Home == nil {
		t.Fatal("Home = nil, want a populated entry")
	}
	if !detail.Home.Exists {
		t.Error("Home.Exists = false, want true")
	}
	if detail.Home.Bytes != 42 {
		t.Errorf("Home.Bytes = %d, want 42", detail.Home.Bytes)
	}
	if detail.Home.Path != homePath {
		t.Errorf("Home.Path = %q, want %q", detail.Home.Path, homePath)
	}
}

func TestWorkspaceHandler_Show_WithRuntimesDir_NotYetCreated(t *testing.T) {
	runtimesDir := filepath.Join(t.TempDir(), "runtimes")
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc, RuntimesDir: runtimesDir}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var detail WorkspaceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Home == nil {
		t.Fatal("Home = nil, want a populated (but Exists=false) entry")
	}
	if detail.Home.Exists {
		t.Error("Home.Exists = true, want false (never dispatched)")
	}
}

func TestWorkspaceHandler_Remove_WithRuntimesDir_DeletesHomeDirAndReportsIt(t *testing.T) {
	runtimesDir := filepath.Join(t.TempDir(), "runtimes")
	homePath, err := resolveWorkspaceHomePath(runtimesDir, "team-a")
	if err != nil {
		t.Fatalf("resolveWorkspaceHomePath: %v", err)
	}
	if err := os.MkdirAll(homePath, 0o755); err != nil {
		t.Fatalf("mkdir home dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "a.txt"), make([]byte, 7), 0o644); err != nil {
		t.Fatalf("write home file: %v", err)
	}

	svc := &fakeWorkspaceService{removeFn: func(string) error { return nil }}
	h := &WorkspaceHandler{Service: svc, RuntimesDir: runtimesDir}
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var resp WorkspaceRemoveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.HomeDeleted {
		t.Error("HomeDeleted = false, want true")
	}
	if resp.HomeBytes != 7 {
		t.Errorf("HomeBytes = %d, want 7", resp.HomeBytes)
	}
	if resp.HomePath != homePath {
		t.Errorf("HomePath = %q, want %q", resp.HomePath, homePath)
	}
	if _, statErr := os.Stat(homePath); !os.IsNotExist(statErr) {
		t.Errorf("home dir still present on disk after remove: stat err=%v", statErr)
	}
}

// TestWorkspaceHandler_Remove_DefaultWorkspace_NeverDeletesHomeDir is defense
// in depth (docs/plans/home-workspace-volume.md PR5: "万一 remove が通っても
// home dir は削除しない多重防御"): even if a bug in the service layer let a
// remove of the reserved default workspace's row through, the handler must
// still refuse to touch its home directory on disk.
func TestWorkspaceHandler_Remove_DefaultWorkspace_NeverDeletesHomeDir(t *testing.T) {
	runtimesDir := filepath.Join(t.TempDir(), "runtimes")
	homePath, err := resolveWorkspaceHomePath(runtimesDir, orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("resolveWorkspaceHomePath: %v", err)
	}
	if err := os.MkdirAll(homePath, 0o755); err != nil {
		t.Fatalf("mkdir home dir: %v", err)
	}

	svc := &fakeWorkspaceService{removeFn: func(string) error { return nil }}
	h := &WorkspaceHandler{Service: svc, RuntimesDir: runtimesDir}
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/"+orchestrator.DefaultWorkspaceSlug, "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var resp WorkspaceRemoveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.HomeDeleted {
		t.Error("HomeDeleted = true, want false")
	}
	if _, statErr := os.Stat(homePath); statErr != nil {
		t.Errorf("default workspace home dir was removed from disk: stat err=%v", statErr)
	}
}

// TestWorkspaceHandler_Remove_HomeDeleteFailure_StillReturns200 pins the
// "part-completed" contract: the workspace row is already gone by the time
// home-directory deletion is attempted, so a deletion failure must not turn
// the whole request into an error response — it is surfaced in the body
// instead (docs/plans/home-workspace-volume.md PR5: "削除失敗... workspace
// 設定 (DB) の削除は先に完了させる (part-completed 状態を許容...)").
func TestWorkspaceHandler_Remove_HomeDeleteFailure_StillReturns200(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("permission-bit test assumes POSIX permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}

	runtimesDir := filepath.Join(t.TempDir(), "runtimes")
	homePath, err := resolveWorkspaceHomePath(runtimesDir, "team-a")
	if err != nil {
		t.Fatalf("resolveWorkspaceHomePath: %v", err)
	}
	homesDir := filepath.Dir(homePath)
	if err := os.MkdirAll(homePath, 0o755); err != nil {
		t.Fatalf("mkdir home dir: %v", err)
	}
	if err := os.Chmod(homesDir, 0o000); err != nil {
		t.Fatalf("chmod homes dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(homesDir, 0o755) })

	svc := &fakeWorkspaceService{removeFn: func(string) error { return nil }}
	h := &WorkspaceHandler{Service: svc, RuntimesDir: runtimesDir}
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on a home-delete failure: %s", w.Code, w.Body.String())
	}

	var resp WorkspaceRemoveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.HomeDeleted {
		t.Error("HomeDeleted = true, want false")
	}
	if resp.HomeDeleteError == "" {
		t.Error("HomeDeleteError = empty, want a non-empty error")
	}
}

func TestWorkspaceHandler_List_StillWorks(t *testing.T) {
	svc := &fakeWorkspaceService{
		listFn: func() ([]*orchestrator.WorkspaceSummary, error) {
			return []*orchestrator.WorkspaceSummary{{ID: "default"}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
}

// --- Export (GET /api/workspaces/{slug}/export, PR5 Step A) ---

func TestWorkspaceHandler_Export_Success(t *testing.T) {
	svc := &fakeWorkspaceService{
		exportFn: func(slug string) ([]byte, string, error) {
			if slug != "team-a" {
				t.Errorf("slug = %q, want team-a", slug)
			}
			return []byte("host_commands:\n  - gh\n"), "rev-7", nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a/export", "", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	if got := w.Header().Get("ETag"); got != `"rev-7"` {
		t.Errorf("ETag = %q, want %q", got, `"rev-7"`)
	}
	if w.Body.String() != "host_commands:\n  - gh\n" {
		t.Errorf("body = %q, want the raw yaml bytes unchanged", w.Body.String())
	}
}

func TestWorkspaceHandler_Export_NotFound(t *testing.T) {
	svc := &fakeWorkspaceService{
		exportFn: func(slug string) ([]byte, string, error) {
			return nil, "", &StatusError{Code: http.StatusNotFound, Message: "not found"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/ghost/export", "", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// --- Import (POST /api/workspaces/import?mode=<create-only|replace>, PR5 Step B) ---

func TestWorkspaceHandler_Import_Success(t *testing.T) {
	var gotSlug, gotMode string
	svc := &fakeWorkspaceService{
		importFn: func(slug string, meta *orchestrator.WorkspaceMeta, mode string) (*WorkspaceDetail, error) {
			gotSlug, gotMode = slug, mode
			return &WorkspaceDetail{Slug: slug, Meta: meta, Revision: "rev-1"}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("slug: team-a\nhost_commands:\n  - gh\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import?mode=replace", "application/yaml", body, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gotSlug != "team-a" {
		t.Errorf("slug passed to service = %q, want team-a", gotSlug)
	}
	if gotMode != "replace" {
		t.Errorf("mode passed to service = %q, want replace (from ?mode= query param)", gotMode)
	}
	if got := w.Header().Get("ETag"); got != `"rev-1"` {
		t.Errorf("ETag = %q, want %q", got, `"rev-1"`)
	}
}

// TestWorkspaceHandler_Import_DefaultsModeWhenQueryParamOmitted pins the
// "import mode の default 値" judgment call (docs/plans/
// workspace-db-consolidation.md leaves this unspecified for PR5; create-only
// is the safe default per the task brief): omitting ?mode= entirely must
// still pass a concrete mode value through to the service layer, not an
// empty string (which ImportWorkspace's own switch would otherwise reject as
// "unknown mode").
func TestWorkspaceHandler_Import_DefaultsModeWhenQueryParamOmitted(t *testing.T) {
	var gotMode string
	svc := &fakeWorkspaceService{
		importFn: func(slug string, meta *orchestrator.WorkspaceMeta, mode string) (*WorkspaceDetail, error) {
			gotMode = mode
			return &WorkspaceDetail{Slug: slug, Meta: meta}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import", "application/yaml", []byte("slug: team-a\n"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotMode != "create-only" {
		t.Errorf("default mode passed to service = %q, want create-only", gotMode)
	}
}

func TestWorkspaceHandler_Import_MissingSlugIs400(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import", "application/yaml", []byte("host_commands: [gh]\n"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Import_BadYAMLIs400(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import", "application/yaml", []byte("slug: team-a\nhostcommands: [gh]\n"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field): %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceHandler_Import_RejectsMultipleDocuments pins that Import
// reuses DecodeWorkspaceCreateStrict (the same strict decode Create uses),
// so a hand-authored two-document import body is rejected rather than
// silently importing only the first document.
func TestWorkspaceHandler_Import_RejectsMultipleDocuments(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	twoDocs := []byte("slug: team-a\nhost_commands: [gh]\n---\nhost_commands: [aws]\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import", "application/yaml", twoDocs, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (multiple documents): %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Import_ConflictPropagates409(t *testing.T) {
	svc := &fakeWorkspaceService{
		importFn: func(slug string, meta *orchestrator.WorkspaceMeta, mode string) (*WorkspaceDetail, error) {
			return nil, &StatusError{Code: http.StatusConflict, Message: "already exists"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import", "application/yaml", []byte("slug: team-a\n"), nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Import_UnknownModePropagates400(t *testing.T) {
	svc := &fakeWorkspaceService{
		importFn: func(slug string, meta *orchestrator.WorkspaceMeta, mode string) (*WorkspaceDetail, error) {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("unknown import mode %q", mode)}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import?mode=bogus", "application/yaml", []byte("slug: team-a\n"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Import_BodyTooLargeIs400(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	big := []byte("slug: team-a\nenv:\n  FOO: \"" + strings.Repeat("x", 2<<20) + "\"\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import", "application/yaml", big, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body too large): %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Apply (docs/plans/volume-only-daemon.md PR-1d codex round-1 Blocker 2/
// Major 1, POST /api/workspaces/apply)
// ---------------------------------------------------------------------------

func TestWorkspaceHandler_Apply_Success(t *testing.T) {
	var gotDryRun bool
	var gotSlug string
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			gotDryRun = dryRun
			gotSlug = apply.Envelope.Metadata.Name
			return &orchestrator.WorkspaceApplyResult{Slug: gotSlug, Created: true, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply", "application/yaml", body, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gotDryRun {
		t.Error("dryRun passed to service = true, want false (no ?dry_run= query param)")
	}
	if gotSlug != "team-a" {
		t.Errorf("slug passed to service = %q, want team-a", gotSlug)
	}
	var result orchestrator.WorkspaceApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.Created {
		t.Error("response Created = false, want true")
	}
}

func TestWorkspaceHandler_Apply_DryRunQueryParamPropagates(t *testing.T) {
	var gotDryRun bool
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			gotDryRun = dryRun
			return &orchestrator.WorkspaceApplyResult{Slug: apply.Envelope.Metadata.Name, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply?dry_run=true", "application/yaml", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !gotDryRun {
		t.Error("dryRun passed to service = false, want true (?dry_run=true)")
	}
}

// TestWorkspaceHandler_Apply_DryRunAcceptsAnyValidBoolean pins the PR-1d
// codex round-2 Major fix: dry_run is parsed with strconv.ParseBool, not a
// bare `== "true"` string compare, so "1"/"True"/"TRUE" etc. are all
// recognized truthy values — not just the exact literal "true".
func TestWorkspaceHandler_Apply_DryRunAcceptsAnyValidBoolean(t *testing.T) {
	for _, raw := range []string{"1", "True", "TRUE", "t"} {
		t.Run(raw, func(t *testing.T) {
			var gotDryRun bool
			svc := &fakeWorkspaceService{
				applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
					gotDryRun = dryRun
					return &orchestrator.WorkspaceApplyResult{Slug: apply.Envelope.Metadata.Name, Meta: &orchestrator.WorkspaceMeta{}}, nil
				},
			}
			h := &WorkspaceHandler{Service: svc}
			body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
			w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply?dry_run="+raw, "application/yaml", body, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if !gotDryRun {
				t.Errorf("dryRun passed to service = false for ?dry_run=%s, want true", raw)
			}
		})
	}
}

// TestWorkspaceHandler_Apply_DryRunInvalidBooleanIs400 pins the other half
// of the same fix: an unparseable dry_run value must fail closed (400), not
// silently fall through to a real commit the way the old `== "true"` check
// did for anything other than the exact string "true".
func TestWorkspaceHandler_Apply_DryRunInvalidBooleanIs400(t *testing.T) {
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			t.Fatal("service.ApplyWorkspace must not be called for an invalid dry_run value")
			return nil, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply?dry_run=maybe", "application/yaml", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// PR-1d codex round-3 Major: an explicitly empty ?dry_run must not bypass
// strconv.ParseBool and silently commit
// ---------------------------------------------------------------------------

// TestWorkspaceHandler_Apply_DryRunPresentButEmptyIs400 pins the round-3
// Major: `?dry_run=` (present, empty value) must be rejected with 400, not
// treated as if the parameter were absent (which would silently commit).
func TestWorkspaceHandler_Apply_DryRunPresentButEmptyIs400(t *testing.T) {
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			t.Fatal("service.ApplyWorkspace must not be called for ?dry_run= (present, empty)")
			return nil, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply?dry_run=", "application/yaml", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceHandler_Apply_DryRunKeyWithNoEqualsIs400 covers the second
// reachable "present but not a valid bool" shape: `?dry_run` with no `=` at
// all (e.g. `?dry_run=${DRY_RUN}` with an unset shell variable collapses to
// this). url.Values.Get also returns "" here, same as ?dry_run=, so this
// must be rejected the same way.
func TestWorkspaceHandler_Apply_DryRunKeyWithNoEqualsIs400(t *testing.T) {
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			t.Fatal("service.ApplyWorkspace must not be called for ?dry_run (no =)")
			return nil, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply?dry_run", "application/yaml", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceHandler_Apply_DryRunAbsentCommits is the regression guard
// alongside the two tests above: omitting ?dry_run entirely must still
// default to a real commit (dryRun=false), unchanged from before this fix.
func TestWorkspaceHandler_Apply_DryRunAbsentCommits(t *testing.T) {
	var gotDryRun bool
	called := false
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			called = true
			gotDryRun = dryRun
			return &orchestrator.WorkspaceApplyResult{Slug: apply.Envelope.Metadata.Name, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply", "application/yaml", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !called {
		t.Fatal("service.ApplyWorkspace was not called")
	}
	if gotDryRun {
		t.Error("dryRun passed to service = true, want false (no ?dry_run param at all)")
	}
}

// TestWorkspaceHandler_Apply_DryRunExplicitFalseCommits pins that
// `?dry_run=false` (present, valid, falsy) also commits — a valid explicit
// "false" must parse cleanly through the same Has+ParseBool path as any
// other valid boolean.
func TestWorkspaceHandler_Apply_DryRunExplicitFalseCommits(t *testing.T) {
	var gotDryRun bool
	called := false
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			called = true
			gotDryRun = dryRun
			return &orchestrator.WorkspaceApplyResult{Slug: apply.Envelope.Metadata.Name, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply?dry_run=false", "application/yaml", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !called {
		t.Fatal("service.ApplyWorkspace was not called")
	}
	if gotDryRun {
		t.Error("dryRun passed to service = true, want false (?dry_run=false)")
	}
}

// ---------------------------------------------------------------------------
// PR-1d codex round-4 Minor: repeated ?dry_run values and query parse errors
// must not fail open to a real commit
// ---------------------------------------------------------------------------

// TestWorkspaceHandler_Apply_DryRunRepeatedValuesIs400 pins the round-4
// Minor: query.Get("dry_run") only ever returns the FIRST value, so
// `?dry_run=false&dry_run=true` silently used "false" and performed a real
// commit even though the caller also (later) said "true" — an ambiguous,
// contradictory request must be rejected instead of picking one arbitrarily.
func TestWorkspaceHandler_Apply_DryRunRepeatedValuesIs400(t *testing.T) {
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			t.Fatal("service.ApplyWorkspace must not be called for ambiguous repeated dry_run values")
			return nil, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply?dry_run=false&dry_run=true", "application/yaml", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceHandler_Apply_QueryParseErrorIs400 pins the round-4 Minor's
// second half: r.URL.Query() silently discards query-parsing errors (e.g. an
// unescaped semicolon separator, rejected by net/url since Go 1.17), which
// can make a key disappear entirely rather than surface the malformed
// request — `?dry_run=true;evil=1` must not silently degrade to "no dry_run
// param at all" (a real commit) but be rejected outright.
func TestWorkspaceHandler_Apply_QueryParseErrorIs400(t *testing.T) {
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			t.Fatal("service.ApplyWorkspace must not be called for a malformed query string")
			return nil, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply?dry_run=true;evil=1", "application/yaml", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceHandler_Apply_AdditionalBindingsWarningInResponse pins the
// PR-1d codex round-2 Minor fix: `boid workspace apply` already warns
// client-side when a document carries a retired spec.additional_bindings
// key, but a caller hitting POST /api/workspaces/apply directly (not
// through the CLI) previously never saw any warning at all — the key was
// silently parsed and discarded. The response body must now carry it too.
func TestWorkspaceHandler_Apply_AdditionalBindingsWarningInResponse(t *testing.T) {
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			if !apply.AdditionalBindingsDropped {
				t.Error("AdditionalBindingsDropped = false, want true")
			}
			return &orchestrator.WorkspaceApplyResult{Slug: apply.Envelope.Metadata.Name, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\nspec:\n  additional_bindings:\n    - source: /host\n      target: /container\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply", "application/yaml", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var result orchestrator.WorkspaceApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("response Warnings is empty, want a warning about additional_bindings")
	}
	if !strings.Contains(result.Warnings[0], "additional_bindings") {
		t.Errorf("Warnings[0] = %q, want it to mention additional_bindings", result.Warnings[0])
	}
}

// TestWorkspaceHandler_Apply_RejectsMultipleDocuments pins that Apply
// (unlike `boid workspace apply`'s own multi-document file support) accepts
// exactly one Workspace document per HTTP request — per-workspace
// transactional atomicity (Blocker 2) is only meaningful per request; the
// CLI is responsible for splitting a multi-document file into one POST per
// document (SplitWorkspaceEnvelopeDocuments).
func TestWorkspaceHandler_Apply_RejectsMultipleDocuments(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	twoDocs := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n---\napiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-b\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply", "application/yaml", twoDocs, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (multiple documents): %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceHandler_Apply_BadYAMLIs400(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply", "application/yaml", []byte("apiVersion: boid.dev/v2\nkind: Workspace\nmetadata:\n  name: team-a\n"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceHandler_Apply_ServiceErrorRollsBackPropagates pins that a
// service-layer failure (the atomic transaction rolling back) surfaces as a
// non-2xx response rather than a 200 with partial results — the CLI relies
// on this to know the workspace's metadata write was NOT left committed.
func TestWorkspaceHandler_Apply_ServiceErrorPropagates(t *testing.T) {
	svc := &fakeWorkspaceService{
		applyFn: func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "boom"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	body := []byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n")
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/apply", "application/yaml", body, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// ExportEnvelope (docs/plans/volume-only-daemon.md PR-1d codex round-1
// Blocker 3, GET /api/workspaces/export?all=true|?name=<slug>)
// ---------------------------------------------------------------------------

func TestWorkspaceHandler_ExportEnvelope_All(t *testing.T) {
	var gotSlugs []string
	svc := &fakeWorkspaceService{
		exportEnvelopesFn: func(slugs []string) ([]byte, error) {
			gotSlugs = slugs
			return []byte("apiVersion: boid.dev/v1\nkind: Workspace\n"), nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/export?all=true", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotSlugs != nil {
		t.Errorf("slugs passed to service = %v, want nil (?all=true means every workspace)", gotSlugs)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
}

func TestWorkspaceHandler_ExportEnvelope_ByName(t *testing.T) {
	var gotSlugs []string
	svc := &fakeWorkspaceService{
		exportEnvelopesFn: func(slugs []string) ([]byte, error) {
			gotSlugs = slugs
			return []byte("apiVersion: boid.dev/v1\nkind: Workspace\n"), nil
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/export?name=team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !equalStringSliceForTest(gotSlugs, []string{"team-a"}) {
		t.Errorf("slugs passed to service = %v, want [team-a]", gotSlugs)
	}
}

func TestWorkspaceHandler_ExportEnvelope_RequiresExactlyOneOfAllOrName(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}

	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/export", "", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("neither ?all nor ?name: status = %d, want 400: %s", w.Code, w.Body.String())
	}

	w2 := doWorkspaceRequest(h.Routes(), http.MethodGet, "/export?all=true&name=team-a", "", nil, nil)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("both ?all and ?name: status = %d, want 400: %s", w2.Code, w2.Body.String())
	}
}

func TestWorkspaceHandler_ExportEnvelope_UnknownSlugIs404(t *testing.T) {
	svc := &fakeWorkspaceService{
		exportEnvelopesFn: func(slugs []string) ([]byte, error) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: "workspace \"ghost\" not found"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/export?name=ghost", "", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}
