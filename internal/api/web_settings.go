package api

import (
	"net/http"
	"sort"

	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/web/templates"
)

// This file implements GET /settings. It renders one server-side snapshot
// (GET /api/config, via SettingsConfigService); every Save action on the
// rendered page is a plain client-side fetch() to the existing
// POST /api/config / /api/config/mutate endpoints — this file never applies
// a config change itself.
//
// Auth: reached only through WebHandler.Routes(), under the same
// loopback-trust-or-paired-session gate every other Web UI page uses.

// SettingsConfigService is the read surface WebHandler.Settings needs to
// prefill /settings' initial render. Deliberately narrower than the full
// api.ConfigService — Apply/Mutate are reached directly by the page's own
// client-side JS, never through this Go handler.
type SettingsConfigService interface {
	// ConfigYAML returns the daemon's current effective config.yaml document
	// and its revision; same contract as api.ConfigService.ConfigYAML.
	ConfigYAML() (data []byte, revision string, err error)
}

// Settings handles GET /settings: fetches the daemon's current config.yaml
// (the same document `boid config get` prints), derives the form-editor
// fields from it, and renders both the form and raw-YAML tabs from that one
// snapshot.
func (h *WebHandler) Settings(w http.ResponseWriter, r *http.Request) {
	if h.ConfigService == nil {
		http.Error(w, "config service not configured", http.StatusInternalServerError)
		return
	}
	data, revision, err := h.ConfigService.ConfigYAML()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view, err := buildSettingsView(data, revision)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.Settings(view).Render(r.Context(), w)
}

// buildSettingsView parses a raw config.yaml document into the
// templates.SettingsView the /settings page renders. Uses the generic
// Tree + dotted-path GetPath (not the typed *config.Config) so a key
// genuinely absent from the document renders as an empty form field
// rather than a defaulted one.
func buildSettingsView(data []byte, revision string) (templates.SettingsView, error) {
	tree, err := config.ParseTree(data)
	if err != nil {
		return templates.SettingsView{}, err
	}

	view := templates.SettingsView{
		Revision:         revision,
		YAML:             string(data),
		ForgeKindOptions: forgeKindOptions(),
	}

	if v, ok := config.GetPath(tree, "sandbox.allowed_domains"); ok {
		view.AllowedDomains = toStringSlice(v)
	}
	if v, ok := config.GetPath(tree, "notify.command"); ok {
		view.NotifyCommand = toStringSlice(v)
	}
	if v, ok := config.GetPath(tree, "web.public_url"); ok {
		if s, ok2 := v.(string); ok2 {
			view.WebPublicURL = s
		}
	}
	if v, ok := config.GetPath(tree, "gateway.forges"); ok {
		if m, ok2 := v.(config.Tree); ok2 {
			view.Forges = toForgeRows(m)
		}
	}

	return view, nil
}

// forgeKindOptions returns config.Schema's "gateway.forges.*.forge" enum
// (github/bitbucket today) by reading the schema directly, rather than
// hard-coding the list here — so the form's dropdown can never silently
// drift out of sync with what POST /api/config/mutate actually accepts.
func forgeKindOptions() []string {
	for _, spec := range config.Schema {
		if spec.Path == "gateway.forges.*.forge" {
			return spec.EnumValues
		}
	}
	return nil
}

// toStringSlice converts a decoded YAML []any (every element expected to be
// a string, per config.KindStringArray) into []string. A non-string element
// is skipped defensively rather than causing a page-render failure — a
// malformed document should surface via the daemon's own validation on
// save, not brick the settings page's GET.
func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// toForgeRows converts gateway.forges' decoded map into a slice of
// ForgeRow, sorted by id for a deterministic render (map iteration order in
// Go is randomized).
func toForgeRows(m config.Tree) []templates.ForgeRow {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	rows := make([]templates.ForgeRow, 0, len(ids))
	for _, id := range ids {
		entry, _ := m[id].(config.Tree)
		rows = append(rows, templates.ForgeRow{
			ID:        id,
			Host:      stringField(entry, "host"),
			Forge:     stringField(entry, "forge"),
			SecretKey: stringField(entry, "secret_key"),
		})
	}
	return rows
}

func stringField(m config.Tree, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}
