package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	removeFn          func(slug string) (*WorkspaceRemoval, error)
	listFn            func() ([]*orchestrator.WorkspaceSummary, error)
	applyFn           func(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error)
	exportEnvelopesFn func(slugs []string) ([]byte, error)
	getInitScriptFn   func(slug string) (*WorkspaceInitScript, error)
	setInitScriptFn   func(slug string, content []byte, ifMatch string, force bool) (*WorkspaceInitScriptResult, error)
}

func (s *fakeWorkspaceService) CreateProject(string) (*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *fakeWorkspaceService) CreateProjectFromGitURL(context.Context, string, string, string) (*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *fakeWorkspaceService) FetchProject(context.Context, string) (*orchestrator.Project, error) {
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
func (s *fakeWorkspaceService) ExplainProject(string) (*orchestrator.ProjectExplain, error) {
	panic("not implemented")
}
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
func (s *fakeWorkspaceService) RemoveWorkspace(slug string) (*WorkspaceRemoval, error) {
	return s.removeFn(slug)
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
func (s *fakeWorkspaceService) GetWorkspaceInitScript(slug string) (*WorkspaceInitScript, error) {
	if s.getInitScriptFn != nil {
		return s.getInitScriptFn(slug)
	}
	panic("not implemented")
}
func (s *fakeWorkspaceService) SetWorkspaceInitScript(slug string, content []byte, ifMatch string, force bool) (*WorkspaceInitScriptResult, error) {
	if s.setInitScriptFn != nil {
		return s.setInitScriptFn(slug, content, ifMatch, force)
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
		removeFn: func(slug string) (*WorkspaceRemoval, error) {
			gotSlug = slug
			return &WorkspaceRemoval{}, nil
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
		removeFn: func(slug string) (*WorkspaceRemoval, error) {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "reserved"}
		},
	}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/default", "", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// --- Workspace HOME size / deletion (docs/plans/home-workspace-volume.md
// Phase 4 PR5, rewired onto the engine's volume API by 論点 a-2 / PR7 of
// docs/plans/workspace-home-volume-persistence.md) ---
//
// The gate these cases exercise changed with the mechanism: it used to be
// "was RuntimesDir wired", it is now "is there an engine handle"
// (WorkspaceHandler.Homes) — the feature's only remaining dependency, since
// nothing here resolves a host path any more. See WorkspaceHomeStore's doc
// comment for the full rationale (論点 a-2, D5).

func TestWorkspaceHandler_Show_NoHomeStore_OmitsHome(t *testing.T) {
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc} // Homes left nil.
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var detail WorkspaceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Home != nil {
		t.Errorf("Home = %+v, want nil (no engine handle wired)", detail.Home)
	}
}

func TestWorkspaceHandler_Show_WithHomeStore_PopulatesHome(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"team-a": 42})
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc, Homes: store}
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
	if want := store.volumeName("team-a"); detail.Home.Volume != want {
		t.Errorf("Home.Volume = %q, want %q", detail.Home.Volume, want)
	}
}

// TestWorkspaceHandler_Show_HomeVolumeIsTheJSONKey pins the wire key itself,
// not just the Go field: 論点 a-2's D4 REPLACES the `path` key rather than
// keeping it alongside, and the CLI renderers this PR updates read the new
// one. A silent rename in only one of the two directions is exactly the drift
// this asserts against.
func TestWorkspaceHandler_Show_HomeVolumeIsTheJSONKey(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"team-a": 42})
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc, Homes: store}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a", "", nil, nil)

	var raw struct {
		Home map[string]any `json:"home"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, ok := raw.Home["volume"]; !ok || got != store.volumeName("team-a") {
		t.Errorf(`home["volume"] = %v (present=%v), want %q`, got, ok, store.volumeName("team-a"))
	}
	if _, ok := raw.Home["path"]; ok {
		t.Error(`home["path"] is still present: PR7 replaces it with "volume" rather than keeping a key that would render a volume name as if it were a path`)
	}
}

func TestWorkspaceHandler_Show_WithHomeStore_NotYetCreated(t *testing.T) {
	svc := &fakeWorkspaceService{
		getFn: func(slug string) (*WorkspaceDetail, error) {
			return &WorkspaceDetail{Slug: slug, Meta: &orchestrator.WorkspaceMeta{}}, nil
		},
	}
	h := &WorkspaceHandler{Service: svc, Homes: newStubHomeStore(map[string]int64{})}
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

func TestWorkspaceHandler_Remove_WithHomeStore_DeletesVolumeAndReportsIt(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"team-a": 7})
	svc := &fakeWorkspaceService{removeFn: func(string) (*WorkspaceRemoval, error) { return &WorkspaceRemoval{}, nil }}
	h := &WorkspaceHandler{Service: svc, Homes: store}
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
	if want := store.volumeName("team-a"); resp.HomeVolume != want {
		t.Errorf("HomeVolume = %q, want %q", resp.HomeVolume, want)
	}
	if _, still := store.homes["team-a"]; still {
		t.Error("home volume still on the engine after remove")
	}
}

// TestWorkspaceHandler_Remove_NoHomeStore_LeavesTheVolumeAlone pins the other
// side of the D5 gate: with no engine handle, remove still succeeds on the
// row and simply reports that no home deletion was attempted.
func TestWorkspaceHandler_Remove_NoHomeStore_LeavesTheVolumeAlone(t *testing.T) {
	svc := &fakeWorkspaceService{removeFn: func(string) (*WorkspaceRemoval, error) { return &WorkspaceRemoval{}, nil }}
	h := &WorkspaceHandler{Service: svc} // Homes left nil.
	w := doWorkspaceRequest(h.Routes(), http.MethodDelete, "/team-a", "", nil, nil)
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
	if resp.HomeVolume != "" {
		t.Errorf("HomeVolume = %q, want empty (nothing was looked at)", resp.HomeVolume)
	}
}

// TestWorkspaceHandler_Remove_DefaultWorkspace_NeverDeletesHomeVolume is
// defense in depth (docs/plans/home-workspace-volume.md PR5: "万一 remove が
// 通っても home dir は削除しない多重防御"): even if a bug in the service layer
// let a remove of the reserved default workspace's row through, the handler
// must still refuse to touch its home.
func TestWorkspaceHandler_Remove_DefaultWorkspace_NeverDeletesHomeVolume(t *testing.T) {
	store := newStubHomeStore(map[string]int64{orchestrator.DefaultWorkspaceSlug: 10})
	svc := &fakeWorkspaceService{removeFn: func(string) (*WorkspaceRemoval, error) { return &WorkspaceRemoval{}, nil }}
	h := &WorkspaceHandler{Service: svc, Homes: store}
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
	if len(store.removed) != 0 {
		t.Errorf("engine VolumeRemove was issued for %v, want none for the reserved default slug", store.removed)
	}
	if _, still := store.homes[orchestrator.DefaultWorkspaceSlug]; !still {
		t.Error("default workspace home volume was removed")
	}
}

// TestWorkspaceHandler_Remove_HomeDeleteFailure_StillReturns200 pins the
// "part-completed" contract: the workspace row is already gone by the time
// home deletion is attempted, so a deletion failure must not turn the whole
// request into an error response — it is surfaced in the body instead
// (docs/plans/home-workspace-volume.md PR5: "削除失敗... workspace 設定 (DB)
// の削除は先に完了させる (part-completed 状態を許容...)"). The engine failure
// modelled here is the one 論点 a-2's D6 measured: a 409 for a volume a
// running job still holds.
func TestWorkspaceHandler_Remove_HomeDeleteFailure_StillReturns200(t *testing.T) {
	store := newStubHomeStore(map[string]int64{"team-a": 10})
	store.removeErrs = map[string]error{
		"team-a": errors.New("volume is being used by the following container(s): abc123"),
	}
	svc := &fakeWorkspaceService{removeFn: func(string) (*WorkspaceRemoval, error) { return &WorkspaceRemoval{}, nil }}
	h := &WorkspaceHandler{Service: svc, Homes: store}
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
	if !strings.Contains(resp.HomeDeleteError, "being used") {
		t.Errorf("HomeDeleteError = %q, want the engine's conflict message passed through", resp.HomeDeleteError)
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

// --- Export (GET /api/workspaces/{slug}/export) retired ---
//
// TestWorkspaceHandler_SlugExportRouteRemoved pins that the meta-format
// single-workspace export endpoint is gone from routing entirely (2026-07-28,
// unified onto the envelope-format GET /api/workspaces/export — see
// ExportEnvelope's tests below): its round trip with `boid workspace import`
// never actually worked (an empty workspace exported as invalid yaml, and a
// non-empty export's "slug:" key was rejected by the CLI's own client-side
// decode). A request against the old path must now 404 — chi's router has
// no route to fall through to, not a handler-level not-found response —
// regardless of whether "team-a" the URL names actually exists.
func TestWorkspaceHandler_SlugExportRouteRemoved(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/team-a/export", "", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route removed): %s", w.Code, w.Body.String())
	}
}

// --- Import (POST /api/workspaces/import) retired ---
//
// TestWorkspaceHandler_ImportRouteRemoved pins that POST /api/workspaces/import
// is gone from routing entirely (2026-07-28, second review round of this PR):
// it was ImportWorkspace's only remaining caller once `boid workspace
// import` (the CLI side) became a deprecated stub that never issues this
// request — a duplicate, zero-reference entry point that contradicted the
// PR's own "one entry point" premise, per the same unification this PR
// already applies to GET /export above. (DecodeWorkspaceCreateStrict, the
// decoder Import used, is NOT similarly orphaned — Create, POST
// /api/workspaces, decodes with it too; only ImportWorkspace and the route
// itself were this endpoint's alone to lose.)
//
// The expected status is 405, NOT 404, and that is a real routing quirk
// worth spelling out rather than a mistake: "/import" is a single path
// segment, same as "/{slug}" (the wildcard route GET/PUT/DELETE-registered
// just below it in Routes()). With the static "/import" POST registration
// gone, a request against this path now falls through to "/{slug}" — chi
// treats "import" as a slug value — which HAS routes, just none for POST,
// so chi's router answers 405 Method Not Allowed rather than 404 (contrast
// TestWorkspaceHandler_SlugExportRouteRemoved below: "/{slug}/export" is
// a TWO-segment path with no other route matching it at all, so THAT one
// really is a plain 404).
//
// GET /api/workspaces/import would, unchanged from before this PR, resolve
// to Show("import") for the same method-specific-shadowing reason — but the
// accurate same-shape example is GET /api/workspaces/apply -> Show("apply")
// (apply only registers POST, so GET falls through to "/{slug}" exactly
// like import now does), or PUT/DELETE /api/workspaces/export ->
// Update/Remove("export") (export only registers GET). GET
// /api/workspaces/export itself is NOT such an example — it resolves to
// ExportEnvelope, not Show, because "/export" DOES have a GET route
// registered (r.Get("/export", h.ExportEnvelope) above); the static/wildcard
// shadowing this comment describes is scoped to the METHOD, not the path,
// and this endpoint's POST registration existing or not was never what
// protected a workspace literally named "import"/"export"/"apply" from it.
func TestWorkspaceHandler_ImportRouteRemoved(t *testing.T) {
	svc := &fakeWorkspaceService{}
	h := &WorkspaceHandler{Service: svc}
	w := doWorkspaceRequest(h.Routes(), http.MethodPost, "/import", "application/yaml", []byte("slug: team-a\nhost_commands:\n  - gh\n"), nil)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (no POST route left; see doc comment for why not 404): %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceRouteShadowing_AcceptedCollision pins the decision recorded in
// Routes()' doc comment: a workspace really can be named "export" or
// "apply", the static routes at the same depth as "/{slug}" really do shadow
// it per-method, and that is ACCEPTED (nose, 2026-07-28) rather than an open
// follow-up. Without this test the decision lives only in prose, and the two
// rejected alternatives — a reserved-word list in ValidWorkspaceSlug, or
// moving the envelope routes off the bare "/{name}" segment — could each be
// re-introduced later as an apparent bugfix with nothing failing to say the
// tradeoff was already weighed.
//
// "import" is in the table too, but as the CONTROL: since #859 removed its
// POST registration it shadows nothing, so its rows pin that a workspace
// named "import" behaves exactly like "team-a". They are what makes the
// "export"/"apply" rows mean something rather than being the only data
// point.
//
// The headline consequence is the GET /export row: `boid workspace show
// export` issues exactly that request and gets ExportEnvelope's 400, not the
// workspace. It is asserted here as the behaviour of record.
func TestWorkspaceRouteShadowing_AcceptedCollision(t *testing.T) {
	// ValidWorkspaceSlug has no reserved-word list, deliberately: these are
	// the names the router shadows (plus the one it used to), and all three
	// remain creatable.
	for _, slug := range []string{"export", "apply", "import"} {
		if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
			t.Errorf("ValidWorkspaceSlug(%q) = %v, want nil (no reserved-word list by design)", slug, err)
		}
	}

	// routed is returned by every recorder below: which service method ran is
	// the fact under test, so none of them needs to produce a valid response.
	routed := errors.New("routed")
	var got string
	newHandler := func() *WorkspaceHandler {
		got = ""
		return &WorkspaceHandler{Service: &fakeWorkspaceService{
			getFn: func(slug string) (*WorkspaceDetail, error) { got = "Show(" + slug + ")"; return nil, routed },
			updateFn: func(slug string, _ *orchestrator.WorkspaceMeta, _ string, _ bool) (*WorkspaceDetail, error) {
				got = "Update(" + slug + ")"
				return nil, routed
			},
			removeFn: func(slug string) (*WorkspaceRemoval, error) { got = "Remove(" + slug + ")"; return nil, routed },
			applyFn: func(*orchestrator.WorkspaceEnvelopeApply, bool) (*orchestrator.WorkspaceApplyResult, error) {
				got = "Apply"
				return nil, routed
			},
			exportEnvelopesFn: func([]string) ([]byte, error) { got = "ExportEnvelope"; return nil, routed },
		}}
	}

	yaml := "application/yaml"
	meta := "host_commands:\n  - gh\n" // valid WorkspaceMeta: anything else 400s in the strict decoder before Update runs
	envelope := "apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-a\n"
	// Every cell of the 3x4 table, so a change to any of them is a visible
	// change rather than a silent one. want is the service method the router
	// dispatched to; wantStatus, when non-zero, is additionally asserted (the
	// 405 cells reach no service method at all, so "" alone would not
	// distinguish "router rejected it" from "handler ran but called nothing").
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		body       string
		want       string
		wantStatus int
	}{
		// "/export" registers GET only: GET is shadowed, POST has no route on
		// either "/export" or "/{slug}" so chi answers 405, and PUT/DELETE
		// fall through to "/{slug}" with slug == "export".
		{"GET /export reaches the envelope export, not Show", http.MethodGet, "/export?all=true", "", "ExportEnvelope", 0},
		{"POST /export is 405, reaching no handler", http.MethodPost, "/export", meta, "", http.StatusMethodNotAllowed},
		{"PUT /export falls through to Update", http.MethodPut, "/export", meta, "Update(export)", 0},
		{"DELETE /export falls through to Remove", http.MethodDelete, "/export", "", "Remove(export)", 0},
		// "/apply" registers POST only, so GET/PUT/DELETE are NOT shadowed.
		{"GET /apply falls through to Show", http.MethodGet, "/apply", "", "Show(apply)", 0},
		{"POST /apply reaches Apply", http.MethodPost, "/apply", envelope, "Apply", 0},
		{"PUT /apply falls through to Update", http.MethodPut, "/apply", meta, "Update(apply)", 0},
		{"DELETE /apply falls through to Remove", http.MethodDelete, "/apply", "", "Remove(apply)", 0},
		// "/import" registers nothing since #859 removed its POST route
		// (2026-07-28), so it shadows NOTHING: these four rows are the control
		// — they are exactly what any ordinary slug does, POST's 405 included
		// (that 405 is "no POST /{slug} exists", not a static route winning;
		// see TestWorkspaceHandler_ImportRouteRemoved).
		{"GET /import falls through to Show", http.MethodGet, "/import", "", "Show(import)", 0},
		{"POST /import is 405 like any other slug", http.MethodPost, "/import", meta, "", http.StatusMethodNotAllowed},
		{"PUT /import falls through to Update", http.MethodPut, "/import", meta, "Update(import)", 0},
		{"DELETE /import falls through to Remove", http.MethodDelete, "/import", "", "Remove(import)", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler()
			var body []byte
			ct := ""
			if tc.body != "" {
				body, ct = []byte(tc.body), yaml
			}
			w := doWorkspaceRequest(h.Routes(), tc.method, tc.path, ct, body, nil)
			if got != tc.want {
				t.Fatalf("%s %s dispatched to %q, want %q", tc.method, tc.path, got, tc.want)
			}
			if tc.wantStatus != 0 && w.Code != tc.wantStatus {
				t.Fatalf("%s %s status = %d, want %d: %s", tc.method, tc.path, w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}

	// The operator-visible edge: `boid workspace show export` issues a bare
	// GET /api/workspaces/export, which ExportEnvelope rejects for having
	// neither ?all=true nor ?name= — the workspace is unreachable through
	// `workspace show`, and never reaches the service at all.
	h := newHandler()
	w := doWorkspaceRequest(h.Routes(), http.MethodGet, "/export", "", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET /export status = %d, want 400 from ExportEnvelope: %s", w.Code, w.Body.String())
	}
	if got != "" {
		t.Fatalf("GET /export reached the service as %q, want no service call (ExportEnvelope rejects before dispatching)", got)
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
