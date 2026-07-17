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
	createFn func(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error)
	getFn    func(slug string) (*WorkspaceDetail, error)
	updateFn func(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error)
	removeFn func(slug string) error
	listFn   func() ([]*orchestrator.WorkspaceSummary, error)
	exportFn func(slug string) ([]byte, string, error)
	importFn func(slug string, meta *orchestrator.WorkspaceMeta, mode string) (*WorkspaceDetail, error)
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
