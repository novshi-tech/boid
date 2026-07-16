package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeConfigService struct {
	kitsDir string
}

func (s *fakeConfigService) KitsDir() string {
	return s.kitsDir
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
