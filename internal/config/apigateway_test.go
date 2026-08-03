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
