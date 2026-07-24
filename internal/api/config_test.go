package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeConfigService struct {
	kitsDir string

	yaml        []byte
	revision    string
	yamlErr     error
	applyErr    error
	applyResp   ConfigApplyResult
	lastApplied []byte
	lastIfMatch string
	lastForce   bool

	mutateErr    error
	mutateResp   ConfigMutateResult
	lastMutateOp ConfigMutateRequest
}

func (s *fakeConfigService) KitsDir() string {
	return s.kitsDir
}

func (s *fakeConfigService) ConfigYAML() ([]byte, string, error) {
	return s.yaml, s.revision, s.yamlErr
}

func (s *fakeConfigService) ApplyConfigYAML(data []byte, ifMatch string, force bool) (ConfigApplyResult, error) {
	s.lastApplied = data
	s.lastIfMatch = ifMatch
	s.lastForce = force
	if s.applyErr != nil {
		return ConfigApplyResult{}, s.applyErr
	}
	return s.applyResp, nil
}

func (s *fakeConfigService) MutateConfig(req ConfigMutateRequest) (ConfigMutateResult, error) {
	s.lastMutateOp = req
	if s.mutateErr != nil {
		return ConfigMutateResult{}, s.mutateErr
	}
	return s.mutateResp, nil
}

// TestConfigHandler_KitsDir pins MAJOR 1 (codex review round 1, docs/plans/
// workspace-db-consolidation.md): GET /api/config/kits-dir must return the
// daemon's effective KitsDir so a CLI client-side helper can resolve kit
// references against the same directory the running daemon actually uses,
// even when the daemon was started with a custom --kits-dir that differs
// from the CLI process's own default derivation.
func TestConfigHandler_KitsDir(t *testing.T) {
	svc := &fakeConfigService{kitsDir: "/custom/kits/dir"}
	h := &ConfigHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/kits-dir", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got configKitsDirResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.KitsDir != "/custom/kits/dir" {
		t.Errorf("KitsDir = %q, want /custom/kits/dir", got.KitsDir)
	}
}

// TestConfigHandler_KitsDir_Empty verifies an unconfigured (empty) KitsDir
// renders as an empty string, not an error.
func TestConfigHandler_KitsDir_Empty(t *testing.T) {
	svc := &fakeConfigService{kitsDir: ""}
	h := &ConfigHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/kits-dir", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got configKitsDirResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.KitsDir != "" {
		t.Errorf("KitsDir = %q, want empty", got.KitsDir)
	}
}

func TestConfigHandler_Get(t *testing.T) {
	svc := &fakeConfigService{yaml: []byte("sandbox:\n  backend: userns\n"), revision: "3"}
	h := &ConfigHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), svc.yaml) {
		t.Errorf("body = %q, want %q", w.Body.String(), svc.yaml)
	}
	// BLOCKER 1 (codex review round 1): GET's ETag mirrors the service's
	// revision, quoted per HTTP convention — the same setWorkspaceETag
	// pattern PUT /api/workspaces/{slug} already established.
	if etag := w.Header().Get("ETag"); etag != `"3"` {
		t.Errorf("ETag = %q, want %q", etag, `"3"`)
	}
}

func TestConfigHandler_Get_ServiceError(t *testing.T) {
	svc := &fakeConfigService{yamlErr: errors.New("boom")}
	h := &ConfigHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestConfigHandler_Apply(t *testing.T) {
	svc := &fakeConfigService{applyResp: ConfigApplyResult{Warnings: []string{"[warning] test"}}}
	h := &ConfigHandler{Service: svc}

	body := []byte("sandbox:\n  backend: userns\n")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("If-Match", `"5"`)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(svc.lastApplied, body) {
		t.Errorf("ApplyConfigYAML received %q, want %q", svc.lastApplied, body)
	}
	// BLOCKER 1: the quoted If-Match header is unquoted before being
	// forwarded to the service, mirroring PUT /api/workspaces/{slug}.
	if svc.lastIfMatch != "5" {
		t.Errorf("lastIfMatch = %q, want unquoted \"5\"", svc.lastIfMatch)
	}
	if svc.lastForce {
		t.Error("lastForce = true, want false (no ?force=true on the request)")
	}
	var got ConfigApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "[warning] test" {
		t.Errorf("Warnings = %v", got.Warnings)
	}
}

// TestConfigHandler_Apply_ForceQueryParam pins the ?force=true convention
// (mirroring PUT /api/workspaces/{slug}?force=true): the handler forwards
// force=true even with no If-Match header at all.
func TestConfigHandler_Apply_ForceQueryParam(t *testing.T) {
	svc := &fakeConfigService{}
	h := &ConfigHandler{Service: svc}

	req := httptest.NewRequest(http.MethodPost, "/?force=true", bytes.NewReader([]byte("web:\n  public_url: https://x.example.com\n")))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !svc.lastForce {
		t.Error("lastForce = false, want true (?force=true on the request)")
	}
}

func TestConfigHandler_Apply_ValidationError(t *testing.T) {
	svc := &fakeConfigService{applyErr: errors.New("unknown config key: foo")}
	h := &ConfigHandler{Service: svc}

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("foo: bar\n")))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["error"] != "unknown config key: foo" {
		t.Errorf("error = %q", got["error"])
	}
}

// TestConfigHandler_Apply_StatusErrorPropagatesItsOwnCode pins BLOCKER 1:
// a *StatusError from ApplyConfigYAML (the 428/412 If-Match contract)
// surfaces with ITS OWN status code, not the generic 400 every other
// ApplyConfigYAML error renders as.
func TestConfigHandler_Apply_StatusErrorPropagatesItsOwnCode(t *testing.T) {
	svc := &fakeConfigService{applyErr: &StatusError{Code: http.StatusPreconditionFailed, Message: "revision mismatch"}}
	h := &ConfigHandler{Service: svc}

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("web:\n  public_url: https://x.example.com\n")))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", w.Code)
	}
}

func TestConfigHandler_Apply_BodyTooLarge(t *testing.T) {
	svc := &fakeConfigService{}
	h := &ConfigHandler{Service: svc}

	huge := bytes.Repeat([]byte("a"), configBodyMaxBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(huge))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestConfigHandler_Mutate_Set pins BLOCKER 1's server-side mutation
// endpoint: `boid config set`'s POST /api/config/mutate call.
func TestConfigHandler_Mutate_Set(t *testing.T) {
	svc := &fakeConfigService{mutateResp: ConfigMutateResult{
		ConfigApplyResult: ConfigApplyResult{Warnings: []string{"[warning] test"}},
		YAML:              []byte("web:\n  public_url: https://x.example.com\n"),
		Revision:          "2",
	}}
	h := &ConfigHandler{Service: svc}

	body := `{"op":"set","key":"web.public_url","value":["https://x.example.com"]}`
	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if svc.lastMutateOp.Op != ConfigMutateSet || svc.lastMutateOp.Key != "web.public_url" {
		t.Errorf("MutateConfig received %+v", svc.lastMutateOp)
	}
	var got ConfigMutateResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Revision != "2" || len(got.Warnings) != 1 {
		t.Errorf("result = %+v", got)
	}
}

// TestConfigHandler_Mutate_Unset pins the unset half.
func TestConfigHandler_Mutate_Unset(t *testing.T) {
	svc := &fakeConfigService{}
	h := &ConfigHandler{Service: svc}

	body := `{"op":"unset","key":"web.public_url"}`
	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if svc.lastMutateOp.Op != ConfigMutateUnset || svc.lastMutateOp.Key != "web.public_url" {
		t.Errorf("MutateConfig received %+v", svc.lastMutateOp)
	}
}

// TestConfigHandler_Mutate_ServiceError pins error propagation for the
// mutate endpoint: a plain error (unknown key, coercion failure) renders
// 400.
func TestConfigHandler_Mutate_ServiceError(t *testing.T) {
	svc := &fakeConfigService{mutateErr: errors.New("unknown config key: sandbox.alowed_domains")}
	h := &ConfigHandler{Service: svc}

	body := `{"op":"set","key":"sandbox.alowed_domains","value":["x"]}`
	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestConfigHandler_Mutate_InvalidJSON pins the request-body decode path.
func TestConfigHandler_Mutate_InvalidJSON(t *testing.T) {
	svc := &fakeConfigService{}
	h := &ConfigHandler{Service: svc}

	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}
