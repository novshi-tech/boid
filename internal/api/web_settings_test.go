package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// stubSettingsConfigService is a minimal SettingsConfigService test double —
// WebHandler.Settings only ever needs the current document + revision (the
// same GET /api/config the CLI's `boid config get` reaches), never
// Apply/Mutate: those are reached directly by the page's own client-side JS
// against the existing /api/config[/mutate] endpoints (PR-1b), not through
// this Go handler.
type stubSettingsConfigService struct {
	data     []byte
	revision string
	err      error
}

func (s *stubSettingsConfigService) ConfigYAML() ([]byte, string, error) {
	return s.data, s.revision, s.err
}

func newTestWebHandlerWithSettings(cfg SettingsConfigService) *chi.Mux {
	h := &WebHandler{ConfigService: cfg}
	r := chi.NewRouter()
	r.Get("/settings", h.Settings)
	return r
}

func TestWebHandler_Settings_Renders(t *testing.T) {
	cfg := &stubSettingsConfigService{
		revision: "rev-abc",
		data: []byte(`
sandbox:
  allowed_domains:
    - .freee.co.jp
    - api.example.com
notify:
  command:
    - notify-send
    - -a
    - boid
web:
  public_url: https://boid.example.com
gateway:
  forges:
    github:
      host: github.com
      forge: github
      secret_key: GITHUB_PAT
`),
	}
	r := newTestWebHandlerWithSettings(cfg)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		".freee.co.jp",
		"api.example.com",
		// Leading "\n" per codex review round 4, PR #831 — a browser drops
		// the FIRST LF immediately after a <textarea> opening tag, so the
		// template always prepends one to keep the argv value byte-exact.
		">\nnotify-send</textarea>",
		">\n-a</textarea>",
		">\nboid</textarea>",
		"https://boid.example.com",
		"github.com",
		"GITHUB_PAT",
		`data-revision="rev-abc"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response should contain %q, got: %s", want, body)
		}
	}
}

func TestWebHandler_Settings_EmptyConfig(t *testing.T) {
	cfg := &stubSettingsConfigService{revision: "empty-rev", data: []byte{}}
	r := newTestWebHandlerWithSettings(cfg)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
}

func TestWebHandler_Settings_ConfigServiceError(t *testing.T) {
	cfg := &stubSettingsConfigService{err: &StatusError{Code: http.StatusInternalServerError, Message: "boom"}}
	r := newTestWebHandlerWithSettings(cfg)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestWebHandler_Settings_NoConfigService(t *testing.T) {
	h := &WebHandler{}
	r := chi.NewRouter()
	r.Get("/settings", h.Settings)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when ConfigService is unwired", w.Code)
	}
}

func TestWebHandler_Routes_IncludesSettings(t *testing.T) {
	svc := &stubWebService{}
	cfg := &stubSettingsConfigService{data: []byte{}}
	h := &WebHandler{Service: svc, ConfigService: cfg}
	r := h.Routes()

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "404 page not found") {
		t.Error("/settings route should be registered on WebHandler.Routes()")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
}

func TestBuildSettingsView_ParsesForgesSortedByID(t *testing.T) {
	data := []byte(`
gateway:
  forges:
    zzz:
      host: zzz.example.com
      forge: github
      secret_key: ZZZ_PAT
    aaa:
      host: aaa.example.com
      forge: bitbucket
      secret_key: AAA_PAT
`)
	view, err := buildSettingsView(data, "rev-1")
	if err != nil {
		t.Fatalf("buildSettingsView: %v", err)
	}
	if len(view.Forges) != 2 {
		t.Fatalf("Forges = %d entries, want 2", len(view.Forges))
	}
	if view.Forges[0].ID != "aaa" || view.Forges[1].ID != "zzz" {
		t.Errorf("Forges should be sorted by ID, got %+v", view.Forges)
	}
}

func TestBuildSettingsView_MissingKeysAreZeroValue(t *testing.T) {
	view, err := buildSettingsView([]byte{}, "rev-empty")
	if err != nil {
		t.Fatalf("buildSettingsView: %v", err)
	}
	if len(view.AllowedDomains) != 0 {
		t.Errorf("AllowedDomains = %v, want empty", view.AllowedDomains)
	}
	if len(view.Forges) != 0 {
		t.Errorf("Forges = %v, want empty", view.Forges)
	}
	if len(view.NotifyCommand) != 0 {
		t.Errorf("NotifyCommand = %v, want empty", view.NotifyCommand)
	}
	if view.WebPublicURL != "" {
		t.Errorf("WebPublicURL = %q, want empty", view.WebPublicURL)
	}
	if view.Revision != "rev-empty" {
		t.Errorf("Revision = %q, want rev-empty", view.Revision)
	}
}

func TestBuildSettingsView_ForgeKindOptionsFromSchema(t *testing.T) {
	view, err := buildSettingsView([]byte{}, "")
	if err != nil {
		t.Fatalf("buildSettingsView: %v", err)
	}
	if len(view.ForgeKindOptions) == 0 {
		t.Fatal("ForgeKindOptions should be populated from config.Schema's gateway.forges.*.forge enum")
	}
	found := map[string]bool{}
	for _, k := range view.ForgeKindOptions {
		found[k] = true
	}
	if !found["github"] || !found["bitbucket"] {
		t.Errorf("ForgeKindOptions = %v, want to include github and bitbucket", view.ForgeKindOptions)
	}
}
