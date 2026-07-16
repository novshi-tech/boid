package api

import (
	"bytes"
	"encoding/json"
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
	createFn func(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error)
	getFn    func(slug string) (*WorkspaceDetail, error)
	updateFn func(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error)
	removeFn func(slug string) error
	listFn   func() ([]*orchestrator.WorkspaceSummary, error)
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
