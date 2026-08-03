package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromPath_Services_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
services:
  myapp:
    base_url: https://myapp-staging.example.com
    auth: { kind: bearer, secret_key: myapp_staging_token }
  myapp-ops:
    base_url: https://ops.example.com/api
    auth: { kind: header, header: X-Api-Key, secret_key: ops_key }
  bitbucket-api:
    base_url: https://api.bitbucket.org/2.0
    auth: { kind: basic, username: x-bitbucket-api-token-auth, secret_key: BB_TOKEN }
  legacy:
    base_url: https://legacy.example.com
    auth: { kind: query, query: api_key, secret_key: legacy_key }
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Services) != 4 {
		t.Fatalf("len(Services) = %d, want 4", len(cfg.Services))
	}
	myapp := cfg.Services["myapp"]
	if myapp.BaseURL != "https://myapp-staging.example.com" {
		t.Errorf("myapp.BaseURL = %q", myapp.BaseURL)
	}
	if myapp.Auth.Kind != "bearer" || myapp.Auth.SecretKey != "myapp_staging_token" {
		t.Errorf("myapp.Auth = %+v", myapp.Auth)
	}
	ops := cfg.Services["myapp-ops"]
	if ops.Auth.Kind != "header" || ops.Auth.Header != "X-Api-Key" || ops.Auth.SecretKey != "ops_key" {
		t.Errorf("myapp-ops.Auth = %+v", ops.Auth)
	}
	bb := cfg.Services["bitbucket-api"]
	if bb.Auth.Kind != "basic" || bb.Auth.Username != "x-bitbucket-api-token-auth" || bb.Auth.SecretKey != "BB_TOKEN" {
		t.Errorf("bitbucket-api.Auth = %+v", bb.Auth)
	}
	legacy := cfg.Services["legacy"]
	if legacy.Auth.Kind != "query" || legacy.Auth.Query != "api_key" || legacy.Auth.SecretKey != "legacy_key" {
		t.Errorf("legacy.Auth = %+v", legacy.Auth)
	}
}

// oauth2 kind is reserved (schema-only) in PR1 (docs/plans/api-gateway.md:
// "oauth2 kindはPR2で足すので今回はconfig schemaにoauth2を予約する程度でよい") —
// config.yaml load must accept it without error even though nothing acts on
// it yet.
func TestLoadFromPath_Services_OAuth2Reserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
services:
  freee:
    base_url: https://api.freee.co.jp
    auth: { kind: oauth2, provider: freee }
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error (oauth2 kind must be accepted as a schema reservation): %v", err)
	}
	freee := cfg.Services["freee"]
	if freee.Auth.Kind != "oauth2" || freee.Auth.Provider != "freee" {
		t.Errorf("freee.Auth = %+v", freee.Auth)
	}
}

func TestLoadFromPath_Services_MissingBaseURLRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
services:
  myapp:
    auth: { kind: bearer, secret_key: k }
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("want error for missing base_url, got nil")
	}
}

func TestLoadFromPath_Services_MalformedBaseURLRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: "not a valid url"
    auth: { kind: bearer, secret_key: k }
`
	err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content)
	if err == nil {
		t.Fatal("want error for a base_url with no scheme/host, got nil")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("error %q does not mention base_url", err.Error())
	}
}

func TestLoadFromPath_Services_BaseURLWithoutSchemeRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: myapp.example.com
    auth: { kind: bearer, secret_key: k }
`
	err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content)
	if err == nil {
		t.Fatal("want error for a base_url with no scheme, got nil")
	}
}

func TestLoadFromPath_Services_UnknownKindRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: hmac, secret_key: k }
`
	err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content)
	if err == nil {
		t.Fatal("want error for unrecognized auth.kind, got nil")
	}
	if !strings.Contains(err.Error(), "hmac") {
		t.Errorf("error %q does not mention the offending kind", err.Error())
	}
}

func TestLoadFromPath_Services_BearerMissingSecretKeyRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: bearer }
`
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
		t.Fatal("want error for bearer with no secret_key, got nil")
	}
}

func TestLoadFromPath_Services_BasicMissingUsernameRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: basic, secret_key: k }
`
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
		t.Fatal("want error for basic with no username, got nil")
	}
}

func TestLoadFromPath_Services_HeaderMissingHeaderNameRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: header, secret_key: k }
`
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
		t.Fatal("want error for header with no header name, got nil")
	}
}

func TestLoadFromPath_Services_QueryMissingQueryNameRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: query, secret_key: k }
`
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
		t.Fatal("want error for query with no query param name, got nil")
	}
}

func TestLoadFromPath_Services_HeaderInvalidNameRejected(t *testing.T) {
	cases := []string{
		"X Api Key",  // whitespace
		"X-Api-Key:", // colon
	}
	for _, header := range cases {
		content := "services:\n  myapp:\n    base_url: https://myapp.example.com\n    auth:\n      kind: header\n      header: \"" + header + "\"\n      secret_key: k\n"
		if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
			t.Errorf("header %q: want error for invalid HTTP header field name, got nil", header)
		}
	}
}

func TestIsValidHTTPHeaderFieldName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"X-Api-Key", true},
		{"Authorization", true},
		{"x-api-key123", true},
		{"X_Api.Key!~", true},
		{"", false},
		{"X Api Key", false},
		{"X-Api-Key:", false},
		{"X-Api-Key\r\n", false},
		{"X-Api-Key\x00", false},
	}
	for _, c := range cases {
		if got := isValidHTTPHeaderFieldName(c.name); got != c.want {
			t.Errorf("isValidHTTPHeaderFieldName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLoadFromPath_Services_HeaderValidNameAccepted(t *testing.T) {
	content := "services:\n  myapp:\n    base_url: https://myapp.example.com\n    auth: { kind: header, header: X-Api-Key, secret_key: k }\n"
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err != nil {
		t.Fatalf("unexpected error for a valid header name: %v", err)
	}
}

func TestLoadFromPath_Services_OAuth2MissingProviderRejected(t *testing.T) {
	content := `
services:
  freee:
    base_url: https://api.freee.co.jp
    auth: { kind: oauth2 }
`
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
		t.Fatal("want error for oauth2 with no provider, got nil")
	}
}

func TestLoadFromPath_Services_DuplicateBaseURLPathTraversalIsNotAConfigConcern(t *testing.T) {
	// base_url itself is trusted daemon config, not sandbox input — nothing
	// to validate here beyond "it parses as a URL". This test exists only
	// to document that boundary; see internal/apigateway/route_test.go for
	// the actual traversal guard (on the REQUEST path, not base_url).
	content := `
services:
  myapp:
    base_url: https://myapp.example.com/a/../b
    auth: { kind: bearer, secret_key: k }
`
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoadFromPath_Services_HTTPBaseURLWarnsButDoesNotFail pins a codex
// review finding: a plain-http base_url means the injected credential
// crosses the network to the upstream in cleartext, which is worth warning
// about — but must NOT be a hard config-load error, since PR1's own primary
// stated use case (internal test/ops APIs) often has no TLS yet in a
// staging environment. See validateServiceConfig's own doc comment for the
// full reasoning.
func TestLoadFromPath_Services_HTTPBaseURLWarnsButDoesNotFail(t *testing.T) {
	buf := captureSlog(t)
	content := `
services:
  myapp:
    base_url: http://myapp-internal.example.com
    auth: { kind: bearer, secret_key: k }
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error (http base_url must be accepted, only warned about): %v", err)
	}
	if cfg.Services["myapp"].BaseURL != "http://myapp-internal.example.com" {
		t.Errorf("Services[myapp].BaseURL = %q", cfg.Services["myapp"].BaseURL)
	}
	if !strings.Contains(buf.String(), "myapp") || !strings.Contains(buf.String(), "cleartext") {
		t.Errorf("expected a warning naming the service and the cleartext risk, log = %q", buf.String())
	}
}

// writeConfigFile writes content to a fresh config.yaml under a new temp
// dir and returns its path — a one-line version of the write half of
// writeAndLoad, for tests (like the one above) that need the *Config
// loadFromPath returns rather than just its error.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAPIGatewayServices_SortedByName(t *testing.T) {
	cfg := &Config{Services: map[string]ServiceConfig{
		"zeta":  {BaseURL: "https://zeta.example.com", Auth: ServiceAuthConfig{Kind: "bearer", SecretKey: "z"}},
		"alpha": {BaseURL: "https://alpha.example.com", Auth: ServiceAuthConfig{Kind: "bearer", SecretKey: "a"}},
	}}
	services := cfg.APIGatewayServices()
	if len(services) != 2 {
		t.Fatalf("len = %d, want 2", len(services))
	}
	if services[0].Name != "alpha" || services[1].Name != "zeta" {
		t.Errorf("order = [%s, %s], want [alpha, zeta]", services[0].Name, services[1].Name)
	}
}

func TestLoadFromPath_ServicesFloor_Valid(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: bearer, secret_key: k }
services_floor:
  - myapp
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ServicesFloor) != 1 || cfg.ServicesFloor[0] != "myapp" {
		t.Errorf("ServicesFloor = %v, want [myapp]", cfg.ServicesFloor)
	}
}

// A services_floor entry naming a service that isn't declared under
// services: is a likely typo, but is deliberately NOT a hard load error
// (mirrors sandbox.allowed_domains' own lenient string-matching floor,
// which never validates entries against anything either) — it just warns.
func TestLoadFromPath_ServicesFloor_UnknownServiceWarns(t *testing.T) {
	buf := captureSlog(t)
	content := `
services_floor:
  - nonexistent
`
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "nonexistent") {
		t.Errorf("expected a warning naming the unconfigured floor entry, log = %q", buf.String())
	}
}

// writeAndLoad is a small helper shared by the negative-validation tests
// above: write content to path and return loadFromPath's error (if any).
func writeAndLoad(t *testing.T, path, content string) error {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadFromPath(path)
	return err
}
