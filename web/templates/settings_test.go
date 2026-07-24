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

// TestSettings_RendersNotifyCommandAndPublicURL pins MAJOR 2 (codex review
// round 1, PR #831): notify.command renders as one row PER argv element
// (never space-joined into a single input, which was lossy for any argument
// containing an embedded space).
func TestSettings_RendersNotifyCommandAndPublicURL(t *testing.T) {
	view := SettingsView{
		NotifyCommand: []string{"notify-send", "-a", "boid"},
		WebPublicURL:  "https://boid.example.com",
	}
	html := renderSettings(t, view)
	// Every row carries a leading "\n" before the argv value (codex review
	// round 4, PR #831) — see TestSettings_NotifyCommandTextareas_HaveLeadingLF
	// for why. ">\nnotify-send</textarea>", not ">notify-send</textarea>".
	for _, want := range []string{">\nnotify-send</textarea>", ">\n-a</textarea>", ">\nboid</textarea>"} {
		if !strings.Contains(html, want) {
			t.Errorf("expected a notify.command argv textarea carrying %s, got: %s", want, html)
		}
	}
	if !strings.Contains(html, `value="https://boid.example.com"`) {
		t.Errorf("expected web.public_url input value, got: %s", html)
	}
}

// TestSettings_NotifyCommandArgWithEmbeddedSpace_SurvivesAsOneField pins the
// exact failure scenario MAJOR 2 fixed: an argument containing an embedded
// space (e.g. `sh -c "echo hello"`'s third element) must round-trip as ONE
// row's content, not be split across two.
func TestSettings_NotifyCommandArgWithEmbeddedSpace_SurvivesAsOneField(t *testing.T) {
	view := SettingsView{NotifyCommand: []string{"sh", "-c", "echo hello"}}
	html := renderSettings(t, view)
	// Leading "\n" per codex review round 4, PR #831 — see
	// TestSettings_NotifyCommandTextareas_HaveLeadingLF.
	if !strings.Contains(html, ">\necho hello</textarea>") {
		t.Errorf("expected the embedded-space argument to render as a single textarea's content, got: %s", html)
	}
}

// TestSettings_NotifyCommandRowsAreTextareasNotInputs pins the round-3 codex
// review fix (PR #831): notify.command argv rows must be <textarea>, not
// <input type="text">. Per the HTML living standard, reading .value off a
// text <input> runs the value sanitization algorithm, which silently strips
// every CR/LF byte — so an existing argv element with an embedded newline
// (e.g. ["sh", "-c", "line1\nline2"]) would come back flattened the moment
// any JS read .value, even on a row the user never edited. <textarea> has
// no such sanitization.
func TestSettings_NotifyCommandRowsAreTextareasNotInputs(t *testing.T) {
	view := SettingsView{NotifyCommand: []string{"sh", "-c", "line1\nline2"}}
	html := renderSettings(t, view)

	idx := strings.Index(html, `id="notify-command-list"`)
	if idx == -1 {
		t.Fatal("expected a #notify-command-list element")
	}
	// Bound the search to the notify-command-list's own markup so we don't
	// accidentally match the unrelated #yaml-textarea further down the page.
	end := strings.Index(html[idx:], "</div>\n\t\t\t\t\t<button")
	if end == -1 {
		end = len(html) - idx
	}
	section := html[idx : idx+end]

	if strings.Contains(section, `<input type="text" class="form-input form-input-sm notify-command-input"`) {
		t.Error("notify.command rows must not render as <input type=\"text\">: browsers strip CR/LF from a text input's .value, silently flattening any existing argv element with an embedded newline")
	}
	if !strings.Contains(section, `<textarea rows="1" class="form-input form-input-sm notify-command-input"`) {
		t.Errorf("expected notify.command rows to render as <textarea rows=\"1\">, got: %s", section)
	}
	if !strings.Contains(section, "line1\nline2") {
		t.Errorf("expected the embedded newline to survive into the rendered textarea content verbatim, got: %s", section)
	}
}

// TestSettings_NotifyCommandTextareas_HaveLeadingLF pins the codex review
// round 4 fix (PR #831): per the HTML living standard, a browser drops the
// FIRST LF immediately following a <textarea> opening tag when parsing it.
// Without a compensating leading "\n" in the rendered HTML, an argv element
// that itself starts with "\n" (e.g. "\necho hello") would render as
// <textarea>\necho hello</textarea> and the browser would expose
// "echo hello" to JS's .value — the leading LF silently and permanently
// lost the moment the page is saved. Every row must render its argv value
// with an extra leading "\n" prepended, whatever the value itself is —
// including an empty string and a value that already starts with "\n".
func TestSettings_NotifyCommandTextareas_HaveLeadingLF(t *testing.T) {
	view := SettingsView{NotifyCommand: []string{"hello", "", "\necho leading-lf"}}
	html := renderSettings(t, view)

	for _, want := range []string{
		">\nhello</textarea>",             // ordinary value: "\n" + "hello"
		">\n</textarea>",                  // empty value: "\n" + ""
		">\n\necho leading-lf</textarea>", // value already starting with "\n": "\n" + "\necho leading-lf"
	} {
		if !strings.Contains(html, want) {
			t.Errorf("expected a notify.command textarea to carry a leading LF before its value (%q), got: %s", want, html)
		}
	}
}

// TestSettings_HasAddNotifyCommandArgButton pins the add/remove-row UI
// (mirroring allowed_domains' pattern) that replaced the old single
// space-separated text input.
func TestSettings_HasAddNotifyCommandArgButton(t *testing.T) {
	html := renderSettings(t, SettingsView{})
	if !strings.Contains(html, "addNotifyCommandRow()") {
		t.Error("expected an add-argument button wired to addNotifyCommandRow()")
	}
}

// TestSettings_ForgesTableCarriesKindOptionsForJS pins MAJOR 1 (codex review
// round 1, PR #831): the forges table exposes the server's fixed kind-option
// list as a data attribute, so a JS-added new row can populate its <select>
// even when gateway.forges (and therefore every existing row) is empty.
func TestSettings_ForgesTableCarriesKindOptionsForJS(t *testing.T) {
	html := renderSettings(t, SettingsView{ForgeKindOptions: []string{"github", "bitbucket"}})
	if !strings.Contains(html, `data-forge-kinds="`) {
		t.Fatal("expected the forges table to carry a data-forge-kinds attribute")
	}
	if !strings.Contains(html, "github") || !strings.Contains(html, "bitbucket") {
		t.Errorf("data-forge-kinds should carry the kind options, got: %s", html)
	}
}

// TestSettings_ForgesTableCarriesKindOptions_EvenWhenForgesEmpty is the
// direct regression test for MAJOR 1's failure scenario: an empty
// gateway.forges must not leave the new-row dropdown with no options.
func TestSettings_ForgesTableCarriesKindOptions_EvenWhenForgesEmpty(t *testing.T) {
	html := renderSettings(t, SettingsView{Forges: nil, ForgeKindOptions: []string{"github", "bitbucket"}})
	if !strings.Contains(html, `data-forge-kinds="`) {
		t.Fatal("expected data-forge-kinds even with zero existing forge rows")
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

// TestSettings_BannersHaveAriaLiveRoles pins MINOR 1 (codex review round 1,
// PR #831): the dynamically-revealed restart banner, conflict banner, and
// both tabs' error/transient banners must carry role/aria-live attributes,
// so a screen reader announces them when JS un-hides them — they render
// `hidden` at page load and only ever become visible via script, which a
// screen reader would otherwise never notice.
func TestSettings_BannersHaveAriaLiveRoles(t *testing.T) {
	html := renderSettings(t, SettingsView{})

	mustContainNear := func(id, attrs string) {
		t.Helper()
		idx := strings.Index(html, `id="`+id+`"`)
		if idx == -1 {
			t.Fatalf("expected an element with id=%q", id)
		}
		// The attributes should appear on the same tag as the id — look at
		// a small window immediately around it rather than requiring exact
		// attribute order.
		start := idx - 200
		if start < 0 {
			start = 0
		}
		end := idx + 200
		if end > len(html) {
			end = len(html)
		}
		window := html[start:end]
		for _, attr := range strings.Split(attrs, ",") {
			if !strings.Contains(window, attr) {
				t.Errorf("element %q should carry %q nearby, got context: %s", id, attr, window)
			}
		}
	}

	mustContainNear("settings-restart-banner", `role="status",aria-live="polite"`)
	mustContainNear("settings-conflict", `role="alert",aria-live="assertive"`)
	mustContainNear("settings-form-error", `role="alert",aria-live="assertive"`)
	mustContainNear("settings-yaml-error", `role="alert",aria-live="assertive"`)
	// codex review round 2 (PR #831): the pre-fix test asserted only the
	// two error/conflict banners above, leaving both tabs' transient
	// "No changes" banners (settings-form-transient/settings-yaml-transient)
	// unchecked even though the markup itself was already correct — a
	// future accidental regression on either transient banner's
	// role/aria-live would have gone undetected by this test.
	mustContainNear("settings-form-transient", `role="status",aria-live="polite"`)
	mustContainNear("settings-yaml-transient", `role="status",aria-live="polite"`)
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
