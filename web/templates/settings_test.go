package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func renderSettings(t *testing.T, view SettingsView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Settings(view).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestSettings_RendersAllowedDomains(t *testing.T) {
	view := SettingsView{
		Revision:       "rev-1",
		AllowedDomains: []string{".freee.co.jp", "api.example.com"},
	}
	html := renderSettings(t, view)
	if !strings.Contains(html, `value=".freee.co.jp"`) {
		t.Errorf("expected an input carrying the first allowed domain, got: %s", html)
	}
	if !strings.Contains(html, `value="api.example.com"`) {
		t.Errorf("expected an input carrying the second allowed domain, got: %s", html)
	}
}

func TestSettings_RendersForgesTable(t *testing.T) {
	view := SettingsView{
		Forges: []ForgeRow{
			{ID: "github", Host: "github.com", Forge: "github", SecretKey: "GITHUB_PAT"},
		},
		ForgeKindOptions: []string{"github", "bitbucket"},
	}
	html := renderSettings(t, view)
	for _, want := range []string{"github", "github.com", "GITHUB_PAT", "bitbucket"} {
		if !strings.Contains(html, want) {
			t.Errorf("forges table should contain %q, got: %s", want, html)
		}
	}
}

func TestSettings_SecretKeyIsNameNotValueNote(t *testing.T) {
	view := SettingsView{
		Forges: []ForgeRow{{ID: "github", Host: "github.com", Forge: "github", SecretKey: "GITHUB_PAT"}},
	}
	html := renderSettings(t, view)
	if !strings.Contains(html, "env var") {
		t.Error("settings page should note that secret_key is an env var NAME, not the value")
	}
	if !strings.Contains(html, "not the value") {
		t.Error("settings page should explicitly say this is not the secret value")
	}
}

func TestSettings_RendersNotifyCommandAndPublicURL(t *testing.T) {
	view := SettingsView{
		NotifyCommand: "notify-send -a boid",
		WebPublicURL:  "https://boid.example.com",
	}
	html := renderSettings(t, view)
	if !strings.Contains(html, `value="notify-send -a boid"`) {
		t.Errorf("expected notify.command input value, got: %s", html)
	}
	if !strings.Contains(html, `value="https://boid.example.com"`) {
		t.Errorf("expected web.public_url input value, got: %s", html)
	}
}

func TestSettings_RendersYAMLTextareaAndRevision(t *testing.T) {
	view := SettingsView{
		Revision: "abc123",
		YAML:     "web:\n  public_url: https://example.com\n",
	}
	html := renderSettings(t, view)
	if !strings.Contains(html, `id="yaml-textarea"`) {
		t.Error("expected a #yaml-textarea element for the raw YAML tab")
	}
	if !strings.Contains(html, "public_url: https://example.com") {
		t.Errorf("YAML textarea should contain the raw config document, got: %s", html)
	}
	if !strings.Contains(html, `data-revision="abc123"`) {
		t.Error("expected the initial revision to be embedded for the JS save flow to round-trip as If-Match")
	}
}

func TestSettings_NoDefaultHarnessField(t *testing.T) {
	// default_harness was removed from the schema in Phase 2.5 PR7
	// (internal/config/schema.go's doc comment) — the settings form must
	// not offer a control for a key the daemon no longer recognizes.
	html := renderSettings(t, SettingsView{})
	if strings.Contains(html, "default_harness") {
		t.Error("settings page must not reference default_harness (removed from schema)")
	}
}

func TestSettings_HasRestartBannerElement(t *testing.T) {
	html := renderSettings(t, SettingsView{})
	if !strings.Contains(html, `id="settings-restart-banner"`) {
		t.Error("expected a persistent restart-required banner element")
	}
	if !strings.Contains(html, "Restart daemon to apply changes") {
		t.Error("banner copy should match the CLI's restart-required wording")
	}
	if !strings.Contains(html, "docker compose -f build/container/compose.yml restart daemon") {
		t.Error("banner should carry the exact restart command the CLI prints")
	}
}

func TestSettings_HasConflictAlertElement(t *testing.T) {
	html := renderSettings(t, SettingsView{})
	if !strings.Contains(html, `id="settings-conflict"`) {
		t.Error("expected a conflict alert element for the 412/428 If-Match failure path")
	}
	if !strings.Contains(html, "reload") {
		t.Error("conflict alert should offer a reload action")
	}
}

func TestSettings_EmptyStateHasAddButtons(t *testing.T) {
	html := renderSettings(t, SettingsView{})
	if !strings.Contains(html, "addDomainRow()") {
		t.Error("expected an add-domain button wired to addDomainRow()")
	}
	if !strings.Contains(html, "addForgeRow()") {
		t.Error("expected an add-forge button wired to addForgeRow()")
	}
}
