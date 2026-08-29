package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateYAML_Valid(t *testing.T) {
	data := []byte(`
sandbox:
  allowed_domains:
    - .freee.co.jp
    - api.example.com
gateway:
  forges:
    github:
      secret_key: gh-pat
`)
	cfg, err := ValidateYAML(data)
	if err != nil {
		t.Fatalf("ValidateYAML: %v", err)
	}
	if len(cfg.Sandbox.AllowedDomains) != 2 {
		t.Errorf("AllowedDomains = %v", cfg.Sandbox.AllowedDomains)
	}
}

func TestValidateYAML_EmptyDocument(t *testing.T) {
	if _, err := ValidateYAML(nil); err != nil {
		t.Fatalf("ValidateYAML(nil): %v", err)
	}
}

func TestValidateYAML_UnknownTopLevelKey(t *testing.T) {
	data := []byte("default_harness: claude-code\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for unknown top-level key default_harness")
	}
	if !strings.Contains(err.Error(), "default_harness") {
		t.Errorf("expected error to name default_harness, got: %v", err)
	}
}

func TestValidateYAML_UnknownNestedKey(t *testing.T) {
	data := []byte("sandbox:\n  alowed_domains:\n    - x.com\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for unknown nested key")
	}
	if !strings.Contains(err.Error(), "sandbox.alowed_domains") {
		t.Errorf("expected error to name sandbox.alowed_domains, got: %v", err)
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("expected suggestion, got: %v", err)
	}
}

func TestValidateYAML_UnknownForgeField(t *testing.T) {
	data := []byte("gateway:\n  forges:\n    github:\n      hots: github.com\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for unknown forge field")
	}
	if !strings.Contains(err.Error(), "gateway.forges.github.hots") {
		t.Errorf("expected error to name the full path, got: %v", err)
	}
}

// TestValidateYAML_SandboxBackend_AcceptedButIgnored pins PR-4's removal of
// sandbox.backend (docs/plans/volume-only-daemon.md §論点e): `boid config
// apply -f`/`edit` must not reject a document that still sets it (any value)
// — see TestLoadFromPath_SandboxBackend_AcceptedButIgnored (config_test.go)
// for the same contract on daemon startup load.
func TestValidateYAML_SandboxBackend_AcceptedButIgnored(t *testing.T) {
	data := []byte("sandbox:\n  backend: bogus\n")
	if _, err := ValidateYAML(data); err != nil {
		t.Fatalf("expected sandbox.backend to be accepted-but-ignored, got error: %v", err)
	}
}

// TestValidateYAML_Services_Accepted pins a regression (found by codex
// review of docs/plans/api-gateway.md PR1): `services:`/`services_floor:`
// parse fine at daemon-startup load (config.Load/loadFromPath) but, without
// a matching internal/config/schema.go entry, `boid config apply -f`/`edit`
// (which validates through ValidateYAML -> ValidateKnownKeys) rejected any
// document containing them as an "unknown config key" — a config.yaml a
// live daemon happily starts with could not be round-tripped through the
// "safe" editing CLI at all.
func TestValidateYAML_Services_Accepted(t *testing.T) {
	data := []byte(`
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: bearer, secret_key: myapp-token }
  legacy:
    base_url: https://legacy.example.com
    auth: { kind: query, query: api_key, secret_key: legacy-key }
services_floor:
  - myapp
`)
	cfg, err := ValidateYAML(data)
	if err != nil {
		t.Fatalf("ValidateYAML: %v", err)
	}
	if len(cfg.Services) != 2 {
		t.Errorf("Services = %v, want 2 entries", cfg.Services)
	}
	if len(cfg.ServicesFloor) != 1 || cfg.ServicesFloor[0] != "myapp" {
		t.Errorf("ServicesFloor = %v, want [myapp]", cfg.ServicesFloor)
	}
}

// TestValidateYAML_Services_RequireAccountAndAllowReadOnlyWrite_Accepted
// pins the PR #1040 opus review finding (item 1, docs/plans/
// api-gateway-credential-accounts.md): services.<name>.require_account
// (D5) and services.<name>.allow_readonly_write both existed on
// config.ServiceConfig and loaded fine at daemon startup (config.Load),
// but neither ever got an internal/config/schema.go entry — so
// ValidateKnownKeys rejected ANY document that set either one as an
// "unknown config key", which meant `boid config apply -f`/`edit`/`set`/
// `unset` could never round-trip a config.yaml that had them, and (per
// MutateConfig's whole-document re-validate) `boid config set` on an
// UNRELATED key failed the same way once either field was present anywhere
// in the document. See
// TestMutateConfig_UnrelatedKey_SucceedsWithRequireAccountPresent
// (internal/server/config_edit_test.go) for that second-order repro.
func TestValidateYAML_Services_RequireAccountAndAllowReadOnlyWrite_Accepted(t *testing.T) {
	data := []byte(`
services:
  freee:
    base_url: https://api.freee.co.jp
    auth: { kind: oauth2, provider: freee }
    require_account: true
    allow_readonly_write: true
`)
	cfg, err := ValidateYAML(data)
	if err != nil {
		t.Fatalf("ValidateYAML: %v", err)
	}
	sc, ok := cfg.Services["freee"]
	if !ok {
		t.Fatal("services.freee missing from decoded config")
	}
	if !sc.RequireAccount {
		t.Error("RequireAccount = false, want true")
	}
	if !sc.AllowReadOnlyWrite {
		t.Error("AllowReadOnlyWrite = false, want true")
	}
}

func TestValidateYAML_UnknownServiceField(t *testing.T) {
	data := []byte("services:\n  myapp:\n    bsae_url: https://myapp.example.com\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for unknown services field")
	}
	if !strings.Contains(err.Error(), "services.myapp.bsae_url") {
		t.Errorf("expected error to name the full path, got: %v", err)
	}
}

func TestValidateYAML_UnknownServiceAuthField(t *testing.T) {
	data := []byte("services:\n  myapp:\n    base_url: https://myapp.example.com\n    auth: { knid: bearer }\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for unknown services auth field")
	}
	if !strings.Contains(err.Error(), "services.myapp.auth.knid") {
		t.Errorf("expected error to name the full path, got: %v", err)
	}
}

func TestValidateYAML_IncompleteCustomForge(t *testing.T) {
	// A custom forge id with no host is rejected by the existing
	// resolveForgeConfig invariant (config.go), exercised here through
	// ValidateYAML's structural decode pass.
	data := []byte("gateway:\n  forges:\n    my-forge:\n      secret_key: x\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for incomplete custom forge entry")
	}
}

func TestValidateYAML_InvalidDomainSyntax(t *testing.T) {
	cases := []string{
		"http://example.com",
		"example.com/path",
		"exa mple.com",
		"example.com:8080",
		".",
		"",
	}
	for _, d := range cases {
		data := []byte("sandbox:\n  allowed_domains:\n    - \"" + d + "\"\n")
		if _, err := ValidateYAML(data); err == nil {
			t.Errorf("expected error for invalid domain entry %q", d)
		}
	}
}

func TestValidateDomainEntry_Valid(t *testing.T) {
	valid := []string{".freee.co.jp", "api.example.com", ".docker.io", "localhost"}
	for _, d := range valid {
		if err := ValidateDomainEntry(d); err != nil {
			t.Errorf("ValidateDomainEntry(%q) = %v, want nil", d, err)
		}
	}
}

// TestValidateDomainEntry_RejectsUnderscore pins MINOR 3 (codex review round
// 1): a domain label containing "_" is not a valid DNS hostname (RFC 1035
// labels are [A-Za-z0-9-], underscore is a common but non-conformant
// extension some non-hostname strings use) and must now be rejected, not
// silently accepted.
func TestValidateDomainEntry_RejectsUnderscore(t *testing.T) {
	cases := []string{"my_service.example.com", "_dmarc.example.com", ".under_score.com"}
	for _, d := range cases {
		if err := ValidateDomainEntry(d); err == nil {
			t.Errorf("ValidateDomainEntry(%q) = nil, want an error (underscore is not a valid hostname character)", d)
		}
	}
}

// TestValidateDomainEntry_RejectsOverlongLabel pins MINOR 3: a single DNS
// label longer than 63 characters is not valid per RFC 1035 §2.3.4.
func TestValidateDomainEntry_RejectsOverlongLabel(t *testing.T) {
	label := strings.Repeat("a", 64) // 64 > the 63-char label limit
	if err := ValidateDomainEntry(label + ".example.com"); err == nil {
		t.Errorf("ValidateDomainEntry with a 64-char label = nil, want an error")
	}
	// Exactly 63 chars is still valid.
	if err := ValidateDomainEntry(strings.Repeat("a", 63) + ".example.com"); err != nil {
		t.Errorf("ValidateDomainEntry with a 63-char label = %v, want nil", err)
	}
}

// TestValidateDomainEntry_RejectsOverlongHost pins MINOR 3: the full
// hostname (excluding a leading "." suffix-match marker) must not exceed
// 253 characters per RFC 1035 §3.1.
func TestValidateDomainEntry_RejectsOverlongHost(t *testing.T) {
	// Build a >253-char host out of many short labels (each label itself
	// under the 63-char cap, so only the *total* length check should fire).
	var labels []string
	for i := 0; i < 40; i++ {
		labels = append(labels, "aaaaaaa") // 7 chars + "." = 8 per label
	}
	host := strings.Join(labels, ".") + ".com" // well over 253 chars total
	if err := ValidateDomainEntry(host); err == nil {
		t.Errorf("ValidateDomainEntry with a >253-char host = nil, want an error")
	}
}

// TestValidateYAML_LegacyGatewayHosts_Accepted pins MAJOR 1 (codex review
// round 1): Config.UnmarshalYAML still accepts the deprecated gateway.hosts
// list for backward compat (config.go), so ValidateYAML — the same
// validation `boid config apply -f`/`set`/`unset`/`edit` all run — must not
// reject a config.yaml that still carries it. Before the fix, pass 1
// (ValidateKnownKeys, schema.go's trie walk) rejected "gateway.hosts"
// outright as an unknown key, making the new editing surface unusable on a
// still-daemon-accepted legacy config.
func TestValidateYAML_LegacyGatewayHosts_Accepted(t *testing.T) {
	data := []byte(`
gateway:
  hosts:
    - host: git.example.com
      forge: github
      secret_key: my-pat
`)
	cfg, err := ValidateYAML(data)
	if err != nil {
		t.Fatalf("ValidateYAML with legacy gateway.hosts: %v", err)
	}
	// UnmarshalYAML folds gateway.hosts into gateway.forges — the resolved
	// *Config carries the merged result, not a raw Hosts field.
	fc, ok := cfg.Gateway.Forges["git.example.com"]
	if !ok {
		t.Fatalf("gateway.hosts entry was not folded into gateway.forges: %+v", cfg.Gateway.Forges)
	}
	if fc.SecretKey != "my-pat" {
		t.Errorf("folded forge secret_key = %q, want my-pat", fc.SecretKey)
	}
}

// TestValidateYAML_LegacyGatewayHosts_ShapeStillValidated pins MAJOR 1's
// other half: recognizing gateway.hosts structurally must not turn off its
// own shape validation (pass 2, Config.UnmarshalYAML's existing per-entry
// host/secret_key/forge completeness checks already enforce this — this
// test just confirms the pass-1 fix didn't accidentally bypass pass 2).
func TestValidateYAML_LegacyGatewayHosts_ShapeStillValidated(t *testing.T) {
	data := []byte(`
gateway:
  hosts:
    - forge: github
      secret_key: my-pat
`) // missing required "host"
	if _, err := ValidateYAML(data); err == nil {
		t.Fatal("expected error for gateway.hosts entry missing \"host\"")
	}
}

// TestValidateYAML_GatewayHosts_NotSettableViaDottedPath pins the other half
// of MAJOR 1's chosen design: gateway.hosts is a recognized-but-read-only
// migration bridge, not a new `boid config set` surface — Set only knows
// how to coerce scalar/array Kinds (schema.go), and KindOpaque intentionally
// has no coercion story.
func TestValidateYAML_GatewayHosts_NotSettableViaDottedPath(t *testing.T) {
	tree := Tree{}
	if _, err := Set(tree, "gateway.hosts", []string{"x"}); err == nil {
		t.Fatal("expected Set(gateway.hosts, ...) to fail — it is read-only via the dotted-path CLI")
	}
}

// TestValidateYAML_GatewayHosts_NotUnsettableViaDottedPath is
// TestValidateYAML_GatewayHosts_NotSettableViaDottedPath's unset half
// (MINOR 1, codex review round 2) — see TestUnset_KindOpaque_Rejected in
// dotted_test.go for the more detailed pinning.
func TestValidateYAML_GatewayHosts_NotUnsettableViaDottedPath(t *testing.T) {
	tree := Tree{}
	if _, err := Unset(tree, "gateway.hosts"); err == nil {
		t.Fatal("expected Unset(gateway.hosts) to fail — it is read-only via the dotted-path CLI")
	}
}

func TestValidateYAML_MapShapeError(t *testing.T) {
	data := []byte("sandbox: not-a-mapping\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for sandbox: <scalar>")
	}
}

// TestValidateEgressProxyPortRange covers the band rules the reference doc
// promises (docs/ja/reference/config-yaml.md: 両方セットで指定してください).
// Exercised through ValidateYAML rather than the unexported helper so the
// test also pins that the rule is actually reachable from a real config
// document.
func TestValidateEgressProxyPortRange(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml    string
		wantErr bool
	}{
		"unset":          {"sandbox:\n  allowed_domains: []\n", false},
		"both set":       {"sandbox:\n  egress_proxy_port_low: 20000\n  egress_proxy_port_high: 20999\n", false},
		"equal ends":     {"sandbox:\n  egress_proxy_port_low: 20000\n  egress_proxy_port_high: 20000\n", false},
		"only low":       {"sandbox:\n  egress_proxy_port_low: 20000\n", true},
		"only high":      {"sandbox:\n  egress_proxy_port_high: 20999\n", true},
		"inverted":       {"sandbox:\n  egress_proxy_port_low: 20999\n  egress_proxy_port_high: 20000\n", true},
		"above max port": {"sandbox:\n  egress_proxy_port_low: 70000\n  egress_proxy_port_high: 70999\n", true},
		"negative":       {"sandbox:\n  egress_proxy_port_low: -1\n  egress_proxy_port_high: 20999\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateYAML([]byte(tc.yaml))
			if tc.wantErr && err == nil {
				t.Errorf("ValidateYAML(%q) = nil, want an error", tc.yaml)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateYAML(%q) = %v, want nil", tc.yaml, err)
			}
		})
	}
}

// TestLoadFromPath_RejectsBadEgressProxyPortRange pins that the band is
// validated on the path `boid start` actually takes. ValidateYAML alone is
// not enough: it only runs for `boid config set/edit/apply`, so a
// hand-edited config.yaml would otherwise start the daemon with a band that
// silently degrades to ephemeral ports.
func TestLoadFromPath_RejectsBadEgressProxyPortRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("sandbox:\n  egress_proxy_port_low: 20000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadFromPath(path); err == nil {
		t.Error("LoadFromPath with only one end of the band set = nil, want an error")
	}
}
