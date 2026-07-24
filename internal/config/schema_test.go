package config

import "testing"

func TestResolveField_KnownPaths(t *testing.T) {
	cases := []struct {
		path   string
		wantOK bool
	}{
		{"sandbox.allowed_domains", true},
		{"sandbox.backend", true},
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

// TestSchema_ReloadClassification pins the PR #830 round-4 simplification
// (nose directive): every leaf that used to be ReloadDynamic
// (sandbox.allowed_domains, notify.command, web.public_url) is now
// ReloadRestartRequired, same as everything else — see ReloadDynamic's own
// doc comment for why. No Schema leaf is ReloadDynamic today.
func TestSchema_ReloadClassification(t *testing.T) {
	restartRequired := map[string]bool{
		"sandbox.allowed_domains":          true,
		"notify.command":                   true,
		"web.public_url":                   true,
		"gateway.forges.github.host":       true,
		"gateway.forges.github.forge":      true,
		"gateway.forges.github.secret_key": true,
		"gc.enabled":                       true,
		"web.http_addr":                    true,
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
	spec, ok := ResolveField("sandbox.backend")
	if !ok {
		t.Fatal("ResolveField(sandbox.backend) not found")
	}
	if spec.Reload != ReloadRetirementWarning {
		t.Errorf("sandbox.backend: reload class = %v, want ReloadRetirementWarning", spec.Reload)
	}
}
