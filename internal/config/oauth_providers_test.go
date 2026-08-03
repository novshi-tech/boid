package config

import (
	"strings"
	"testing"
)

// TestLoadFromPath_OAuthProviders_Valid pins docs/plans/api-gateway.md
// §6/§論点4's PR2 config surface: the oauth_providers: block, keyed by
// provider name, holding token_endpoint/client_id/client_secret_key/scopes
// — client_secret itself never appears here, only a secret-store reference.
func TestLoadFromPath_OAuthProviders_Valid(t *testing.T) {
	content := `
oauth_providers:
  freee:
    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token
    client_id: freee-client-id
    client_secret_key: freee-oauth-client-secret
    scopes: [read, write]
services:
  freee:
    base_url: https://api.freee.co.jp
    auth: { kind: oauth2, provider: freee }
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.OAuthProviders) != 1 {
		t.Fatalf("len(OAuthProviders) = %d, want 1", len(cfg.OAuthProviders))
	}
	freee := cfg.OAuthProviders["freee"]
	if freee.TokenEndpoint != "https://accounts.secure.freee.co.jp/public_api/token" {
		t.Errorf("TokenEndpoint = %q", freee.TokenEndpoint)
	}
	if freee.ClientID != "freee-client-id" {
		t.Errorf("ClientID = %q", freee.ClientID)
	}
	if freee.ClientSecretKey != "freee-oauth-client-secret" {
		t.Errorf("ClientSecretKey = %q", freee.ClientSecretKey)
	}
	if len(freee.Scopes) != 2 || freee.Scopes[0] != "read" || freee.Scopes[1] != "write" {
		t.Errorf("Scopes = %v, want [read write]", freee.Scopes)
	}
}

// TestLoadFromPath_OAuthProviders_ClientSecretKeyOptional pins that a
// public client (PKCE, no client_secret — reserved for PR3's login flow) is
// a valid oauth_providers entry: only token_endpoint and client_id are
// required.
func TestLoadFromPath_OAuthProviders_ClientSecretKeyOptional(t *testing.T) {
	content := `
oauth_providers:
  github:
    token_endpoint: https://github.com/login/oauth/access_token
    client_id: gh-client-id
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error (client_secret_key must be optional for a public client): %v", err)
	}
	if cfg.OAuthProviders["github"].ClientSecretKey != "" {
		t.Errorf("ClientSecretKey = %q, want empty", cfg.OAuthProviders["github"].ClientSecretKey)
	}
}

func TestLoadFromPath_OAuthProviders_MissingTokenEndpointRejected(t *testing.T) {
	content := `
oauth_providers:
  freee:
    client_id: cid
`
	if _, err := loadFromPath(writeConfigFile(t, content)); err == nil {
		t.Fatal("want error for missing token_endpoint, got nil")
	}
}

func TestLoadFromPath_OAuthProviders_MalformedTokenEndpointRejected(t *testing.T) {
	content := `
oauth_providers:
  freee:
    token_endpoint: "not a valid url"
    client_id: cid
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for a token_endpoint with no scheme/host, got nil")
	}
	if !strings.Contains(err.Error(), "token_endpoint") {
		t.Errorf("error %q does not mention token_endpoint", err.Error())
	}
}

// TestLoadFromPath_OAuthProviders_HTTPTokenEndpointRejected pins that,
// unlike services.*.base_url, there is no allow_insecure escape hatch for
// oauth_providers.*.token_endpoint: it always targets a real OAuth2
// provider (daemon-to-provider, never a sandbox-facing or internal-test
// endpoint), so plaintext is never a legitimate use case here.
func TestLoadFromPath_OAuthProviders_HTTPTokenEndpointRejected(t *testing.T) {
	content := `
oauth_providers:
  freee:
    token_endpoint: http://accounts.secure.freee.co.jp/public_api/token
    client_id: cid
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for a non-https token_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error %q does not mention https", err.Error())
	}
}

func TestLoadFromPath_OAuthProviders_MissingClientIDRejected(t *testing.T) {
	content := `
oauth_providers:
  freee:
    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token
`
	if _, err := loadFromPath(writeConfigFile(t, content)); err == nil {
		t.Fatal("want error for missing client_id, got nil")
	}
}

func TestLoadFromPath_OAuthProviders_WhitespacePaddedNameRejected(t *testing.T) {
	content := "oauth_providers:\n  \" freee\":\n    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token\n    client_id: cid\n"
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for a whitespace-padded provider name, got nil")
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("error should mention whitespace, got: %v", err)
	}
}

func TestLoadFromPath_OAuthProviders_EmptyNameRejected(t *testing.T) {
	content := "oauth_providers:\n  \"\":\n    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token\n    client_id: cid\n"
	if _, err := loadFromPath(writeConfigFile(t, content)); err == nil {
		t.Fatal("want error for an empty provider name, got nil")
	}
}

// TestLoadFromPath_Services_OAuth2ProviderReferenceNotCrossValidated
// documents a deliberate design choice: a services.*.auth.provider naming a
// provider oauth_providers: does not declare is NOT a config-load error —
// mirrors services_floor's own leniency (config.go) and, more fundamentally,
// the fact that NO secret-store-shaped reference in this package (secret_key
// included) is validated against its actual target at config-load time.
// Unlike a secret_key miss (only discoverable at request time regardless,
// since the secret store isn't consulted here), an oauth_providers
// cross-reference COULD technically be checked eagerly since both blocks
// live in the same document — but doing so would special-case oauth2 alone
// among every other reference shape this file has, for a mistake that
// already surfaces as a clear, immediate 502 the first time the service is
// actually dispatched to (apigateway.OAuth2TokenSource.AccessToken's "oauth2
// provider %q is not configured" error) — the same request-time-only
// contract every other unconfigured-reference case in this gateway already
// has.
func TestLoadFromPath_Services_OAuth2ProviderReferenceNotCrossValidated(t *testing.T) {
	content := `
services:
  freee:
    base_url: https://api.freee.co.jp
    auth: { kind: oauth2, provider: not-declared-anywhere }
`
	if _, err := loadFromPath(writeConfigFile(t, content)); err != nil {
		t.Fatalf("unexpected error (provider cross-reference is deliberately not config-load validated): %v", err)
	}
}

func TestAPIGatewayOAuthProviders_SortedByName(t *testing.T) {
	cfg := &Config{OAuthProviders: map[string]OAuthProviderConfig{
		"zeta":  {TokenEndpoint: "https://zeta.example.com/token", ClientID: "z"},
		"alpha": {TokenEndpoint: "https://alpha.example.com/token", ClientID: "a"},
	}}
	providers := cfg.APIGatewayOAuthProviders()
	if len(providers) != 2 {
		t.Fatalf("len = %d, want 2", len(providers))
	}
	if providers[0].Name != "alpha" || providers[1].Name != "zeta" {
		t.Errorf("order = [%s, %s], want [alpha, zeta]", providers[0].Name, providers[1].Name)
	}
}

// TestAPIGatewayOAuthProviders_FieldMapping pins that every field of
// OAuthProviderConfig (this package's config.yaml-decoded shape) survives
// the conversion into apigateway.OAuthProviderConfig unchanged —
// TestAPIGatewayOAuthProviders_SortedByName above only asserts on Name/
// ordering, which would not catch a copy-paste bug that dropped or
// mismapped one of the other fields (e.g. ClientSecretKey or Scopes).
func TestAPIGatewayOAuthProviders_FieldMapping(t *testing.T) {
	cfg := &Config{OAuthProviders: map[string]OAuthProviderConfig{
		"freee": {
			TokenEndpoint:   "https://accounts.secure.freee.co.jp/public_api/token",
			ClientID:        "freee-client-id",
			ClientSecretKey: "freee-oauth-client-secret",
			Scopes:          []string{"read", "write"},
		},
	}}
	providers := cfg.APIGatewayOAuthProviders()
	if len(providers) != 1 {
		t.Fatalf("len = %d, want 1", len(providers))
	}
	p := providers[0]
	if p.Name != "freee" {
		t.Errorf("Name = %q, want %q", p.Name, "freee")
	}
	if p.TokenEndpoint != "https://accounts.secure.freee.co.jp/public_api/token" {
		t.Errorf("TokenEndpoint = %q", p.TokenEndpoint)
	}
	if p.ClientID != "freee-client-id" {
		t.Errorf("ClientID = %q", p.ClientID)
	}
	if p.ClientSecretKey != "freee-oauth-client-secret" {
		t.Errorf("ClientSecretKey = %q", p.ClientSecretKey)
	}
	if len(p.Scopes) != 2 || p.Scopes[0] != "read" || p.Scopes[1] != "write" {
		t.Errorf("Scopes = %v, want [read write]", p.Scopes)
	}
}
