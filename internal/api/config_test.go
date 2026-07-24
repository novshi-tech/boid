package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeConfigService struct {
	kitsDir string

	yaml        []byte
	yamlErr     error
	applyErr    error
	applyResp   ConfigApplyResult
	lastApplied []byte
}

func (s *fakeConfigService) KitsDir() string {
	return s.kitsDir
}

func (s *fakeConfigService) ConfigYAML() ([]byte, error) {
	return s.yaml, s.yamlErr
}

func (s *fakeConfigService) ApplyConfigYAML(data []byte) (ConfigApplyResult, error) {
	s.lastApplied = data
	if s.applyErr != nil {
		return ConfigApplyResult{}, s.applyErr
	}
	return s.applyResp, nil
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
	svc := &fakeConfigService{yaml: []byte("sandbox:\n  backend: userns\n")}
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
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(svc.lastApplied, body) {
		t.Errorf("ApplyConfigYAML received %q, want %q", svc.lastApplied, body)
	}
	var got ConfigApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "[warning] test" {
		t.Errorf("Warnings = %v", got.Warnings)
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
