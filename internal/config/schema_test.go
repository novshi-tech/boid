package config

import "testing"

func TestResolveField_KnownPaths(t *testing.T) {
	cases := []struct {
		path   string
		wantOK bool
	}{
		{"sandbox.allowed_domains", true},
		{"sandbox.backend", true},
		{"log.level", true},
		{"web.public_url", true},
		{"notify.command", true},
		{"gc.enabled", true},
		{"gc.interval", true},
		{"task_ask.disconnect_grace", true},
		{"gateway.forges.github.host", true},
		{"gateway.forges.github-enterprise.secret_key", true},
		{"gateway.forges.github", false}, // whole entry: not a Set/Get leaf, see IsForgeEntryPath
		{"gateway.forges", false},        // container, not a leaf
		{"gateway.hosts", true},          // MAJOR 1: recognized (KindOpaque), read-only legacy migration bridge
		{"default_harness", false},       // removed in Phase 2.5 PR7 — deliberately absent
		{"sandbox.alowed_domains", false},
		// docs/plans/api-gateway.md §2/§3.
		{"services.myapp.base_url", true},
		{"services.myapp.allow_insecure", true},
		{"services.myapp.auth.kind", true},
		{"services.myapp.auth.secret_key", true},
		{"services.myapp", false}, // whole entry, not a Set/Get leaf — same as gateway.forges.github
		{"services_floor", true},
		// docs/plans/api-gateway.md §6/§論点4, PR2.
		{"oauth_providers.freee.token_endpoint", true},
		{"oauth_providers.freee.client_id", true},
		{"oauth_providers.freee.client_secret_key", true},
		{"oauth_providers.freee.scopes", true},
		{"oauth_providers.freee", false}, // whole entry, not a Set/Get leaf — same as services.myapp
		// docs/plans/api-gateway.md §7, PR3 (login flow).
		{"oauth_providers.freee.flow", true},
		{"oauth_providers.freee.authorization_endpoint", true},
		{"oauth_providers.freee.device_authorization_endpoint", true},
		{"oauth_providers.google.authorize_params.access_type", true},
		// docs/plans/signal-ingest-detailed-design.md §6.1 (PR-4).
		{"integrations.dir", true},
		{"integrations", false}, // container, not a leaf
		{"services.myapp.uses", true},
		{"services.myapp.endpoint", true},
		{"services.myapp.credentials.token", true},
	}
	for _, tc := range cases {
		_, ok := ResolveField(tc.path)
		if ok != tc.wantOK {
			t.Errorf("ResolveField(%q) ok = %v, want %v", tc.path, ok, tc.wantOK)
		}
	}
}

func TestIsForgeEntryPath(t *testing.T) {
	id, ok := IsForgeEntryPath("gateway.forges.github")
	if !ok || id != "github" {
		t.Errorf("IsForgeEntryPath(gateway.forges.github) = (%q, %v), want (github, true)", id, ok)
	}
	if _, ok := IsForgeEntryPath("gateway.forges.github.host"); ok {
		t.Errorf("IsForgeEntryPath(gateway.forges.github.host) should be false (leaf, not entry)")
	}
	if _, ok := IsForgeEntryPath("gateway.forges"); ok {
		t.Errorf("IsForgeEntryPath(gateway.forges) should be false (no id segment)")
	}
	if _, ok := IsForgeEntryPath("sandbox.allowed_domains"); ok {
		t.Errorf("IsForgeEntryPath(sandbox.allowed_domains) should be false")
	}
}

// TestIsServiceEntryPath mirrors TestIsForgeEntryPath for
// services.<name> (docs/plans/api-gateway.md §2).
func TestIsServiceEntryPath(t *testing.T) {
	name, ok := IsServiceEntryPath("services.myapp")
	if !ok || name != "myapp" {
		t.Errorf("IsServiceEntryPath(services.myapp) = (%q, %v), want (myapp, true)", name, ok)
	}
	if _, ok := IsServiceEntryPath("services.myapp.base_url"); ok {
		t.Errorf("IsServiceEntryPath(services.myapp.base_url) should be false (leaf, not entry)")
	}
	if _, ok := IsServiceEntryPath("services"); ok {
		t.Errorf("IsServiceEntryPath(services) should be false (no name segment)")
	}
	if _, ok := IsServiceEntryPath("sandbox.allowed_domains"); ok {
		t.Errorf("IsServiceEntryPath(sandbox.allowed_domains) should be false")
	}
	if _, ok := IsServiceEntryPath("gateway.forges.github"); ok {
		t.Errorf("IsServiceEntryPath(gateway.forges.github) should be false")
	}
}

// TestIsOAuthProviderEntryPath mirrors TestIsServiceEntryPath for
// oauth_providers.<name> (docs/plans/api-gateway.md §6/§論点4, PR2).
func TestIsOAuthProviderEntryPath(t *testing.T) {
	name, ok := IsOAuthProviderEntryPath("oauth_providers.freee")
	if !ok || name != "freee" {
		t.Errorf("IsOAuthProviderEntryPath(oauth_providers.freee) = (%q, %v), want (freee, true)", name, ok)
	}
	if _, ok := IsOAuthProviderEntryPath("oauth_providers.freee.token_endpoint"); ok {
		t.Errorf("IsOAuthProviderEntryPath(oauth_providers.freee.token_endpoint) should be false (leaf, not entry)")
	}
	if _, ok := IsOAuthProviderEntryPath("oauth_providers"); ok {
		t.Errorf("IsOAuthProviderEntryPath(oauth_providers) should be false (no name segment)")
	}
	if _, ok := IsOAuthProviderEntryPath("sandbox.allowed_domains"); ok {
		t.Errorf("IsOAuthProviderEntryPath(sandbox.allowed_domains) should be false")
	}
	if _, ok := IsOAuthProviderEntryPath("services.myapp"); ok {
		t.Errorf("IsOAuthProviderEntryPath(services.myapp) should be false")
	}
}

// TestSchema_ReloadClassification pins the PR #830 round-4 simplification
// (nose directive): every leaf that used to be ReloadDynamic
// (sandbox.allowed_domains, notify.command, web.public_url) is now
// ReloadRestartRequired, same as everything else — see ReloadDynamic's own
// doc comment for why. No Schema leaf is ReloadDynamic today.
func TestSchema_ReloadClassification(t *testing.T) {
	restartRequired := map[string]bool{
		"sandbox.allowed_domains":              true,
		"notify.command":                       true,
		"web.public_url":                       true,
		"gateway.forges.github.host":           true,
		"gateway.forges.github.forge":          true,
		"gateway.forges.github.secret_key":     true,
		"gc.enabled":                           true,
		"web.http_addr":                        true,
		"log.level":                            true,
		"services.myapp.base_url":              true,
		"services_floor":                       true,
		"oauth_providers.freee.token_endpoint": true,
	}
	for path := range restartRequired {
		spec, ok := ResolveField(path)
		if !ok {
			t.Fatalf("ResolveField(%q) not found", path)
		}
		if spec.Reload != ReloadRestartRequired {
			t.Errorf("%s: reload class = %v, want ReloadRestartRequired", path, spec.Reload)
		}
	}
	// sandbox.backend (PR-4, docs/plans/volume-only-daemon.md §論点e): now
	// KindOpaque, same read-only-legacy-migration-bridge shape as
	// gateway.hosts, so it takes the same ReloadRestartRequired class those
	// leaves share (see restartFieldExtractorExemptions in
	// internal/server/config_edit.go for why it needs no live-value
	// extractor).
	spec, ok := ResolveField("sandbox.backend")
	if !ok {
		t.Fatal("ResolveField(sandbox.backend) not found")
	}
	if spec.Kind != KindOpaque {
		t.Errorf("sandbox.backend: kind = %v, want KindOpaque", spec.Kind)
	}
	if spec.Reload != ReloadRestartRequired {
		t.Errorf("sandbox.backend: reload class = %v, want ReloadRestartRequired", spec.Reload)
	}
}

// TestSchema_OAuthProvidersFlow_IsEnumWithThreeValues pins
// oauth_providers.*.flow's EnumValues against apigateway.ValidLoginFlows —
// docs/plans/api-gateway.md §7, PR3.
func TestSchema_OAuthProvidersFlow_IsEnumWithThreeValues(t *testing.T) {
	spec, ok := ResolveField("oauth_providers.freee.flow")
	if !ok {
		t.Fatal("ResolveField(oauth_providers.freee.flow) not found")
	}
	if spec.Kind != KindEnum {
		t.Errorf("kind = %v, want KindEnum", spec.Kind)
	}
	want := map[string]bool{"device": true, "loopback": true, "manual": true}
	if len(spec.EnumValues) != len(want) {
		t.Fatalf("EnumValues = %v, want exactly %v", spec.EnumValues, want)
	}
	for _, v := range spec.EnumValues {
		if !want[v] {
			t.Errorf("unexpected EnumValues entry %q", v)
		}
	}
}

// TestSchema_OAuthProvidersGrant_IsEnumWithTwoValues pins
// oauth_providers.*.grant's EnumValues against apigateway.ValidOAuthGrants —
// docs/plans/api-gateway.md §6-補, PR4.
func TestSchema_OAuthProvidersGrant_IsEnumWithTwoValues(t *testing.T) {
	spec, ok := ResolveField("oauth_providers.az.grant")
	if !ok {
		t.Fatal("ResolveField(oauth_providers.az.grant) not found")
	}
	if spec.Kind != KindEnum {
		t.Errorf("kind = %v, want KindEnum", spec.Kind)
	}
	want := map[string]bool{"authorization_code": true, "client_credentials": true}
	if len(spec.EnumValues) != len(want) {
		t.Fatalf("EnumValues = %v, want exactly %v", spec.EnumValues, want)
	}
	for _, v := range spec.EnumValues {
		if !want[v] {
			t.Errorf("unexpected EnumValues entry %q", v)
		}
	}
}

// TestSchema_LogLevel_IsEnumWithLogLevelNames pins that "log.level"'s schema
// entry is a KindEnum whose EnumValues is exactly LogLevelNames (internal/
// config/log_level.go) — so `boid config set log.level <bogus>`'s
// dotted.go-side validation (coerceValues) can never silently drift out of
// sync with what ParseLogLevel/Config.UnmarshalYAML actually accept.
func TestSchema_LogLevel_IsEnumWithLogLevelNames(t *testing.T) {
	spec, ok := ResolveField("log.level")
	if !ok {
		t.Fatal("ResolveField(log.level) not found")
	}
	if spec.Kind != KindEnum {
		t.Errorf("log.level: kind = %v, want KindEnum", spec.Kind)
	}
	if len(spec.EnumValues) != len(LogLevelNames) {
		t.Fatalf("log.level: EnumValues = %v, want %v", spec.EnumValues, LogLevelNames)
	}
	for i, v := range LogLevelNames {
		if spec.EnumValues[i] != v {
			t.Errorf("log.level: EnumValues[%d] = %q, want %q", i, spec.EnumValues[i], v)
		}
	}
}
