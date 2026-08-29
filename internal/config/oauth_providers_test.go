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

// TestLoadFromPath_OAuthProviders_AtSignInNameRejected pins docs/plans/
// api-gateway-credential-accounts.md D1: an oauth_providers.<name> config
// key containing "@" is rejected at config-load time, since PR-2's
// account-qualified oauth2 credential key ("oauth2:<provider>@<account>:...")
// reserves "@" to separate the provider name from an account qualifier.
func TestLoadFromPath_OAuthProviders_AtSignInNameRejected(t *testing.T) {
	content := "oauth_providers:\n  \"freee@ubs\":\n    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token\n    client_id: cid\n"
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for a provider name containing \"@\", got nil")
	}
	if !strings.Contains(err.Error(), "@") {
		t.Errorf("error should mention \"@\", got: %v", err)
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

// TestLoadFromPath_OAuthProviders_FlowOptional pins that an existing
// PR2-era config.yaml — token_endpoint/client_id/client_secret_key/scopes
// only, no flow at all — keeps loading unmodified once PR3 ships (docs/plans/
// api-gateway.md §7): Flow is deliberately optional, not a required field
// retroactively imposed on every entry.
func TestLoadFromPath_OAuthProviders_FlowOptional(t *testing.T) {
	content := `
oauth_providers:
  freee:
    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token
    client_id: freee-client-id
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error (flow must be optional): %v", err)
	}
	if cfg.OAuthProviders["freee"].Flow != "" {
		t.Errorf("Flow = %q, want empty", cfg.OAuthProviders["freee"].Flow)
	}
}

// TestLoadFromPath_OAuthProviders_DeviceFlow pins the device-flow shape
// (docs/plans/api-gateway.md §7 flow table: Microsoft/GitHub).
func TestLoadFromPath_OAuthProviders_DeviceFlow(t *testing.T) {
	content := `
oauth_providers:
  github:
    token_endpoint: https://github.com/login/oauth/access_token
    client_id: gh-client-id
    flow: device
    device_authorization_endpoint: https://github.com/login/device/code
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gh := cfg.OAuthProviders["github"]
	if gh.Flow != "device" {
		t.Errorf("Flow = %q, want device", gh.Flow)
	}
	if gh.DeviceAuthorizationEndpoint != "https://github.com/login/device/code" {
		t.Errorf("DeviceAuthorizationEndpoint = %q", gh.DeviceAuthorizationEndpoint)
	}
}

func TestLoadFromPath_OAuthProviders_DeviceFlow_MissingDeviceAuthorizationEndpointRejected(t *testing.T) {
	content := `
oauth_providers:
  github:
    token_endpoint: https://github.com/login/oauth/access_token
    client_id: gh-client-id
    flow: device
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for a device flow with no device_authorization_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "device_authorization_endpoint") {
		t.Errorf("error %q does not mention device_authorization_endpoint", err.Error())
	}
}

// TestLoadFromPath_OAuthProviders_LoopbackFlow pins the loopback-flow shape
// (docs/plans/api-gateway.md §7 flow table: Google/Atlassian), including
// authorize_params (the Google access_type/prompt quirk).
func TestLoadFromPath_OAuthProviders_LoopbackFlow(t *testing.T) {
	content := `
oauth_providers:
  google:
    token_endpoint: https://oauth2.googleapis.com/token
    client_id: google-client-id
    flow: loopback
    authorization_endpoint: https://accounts.google.com/o/oauth2/v2/auth
    authorize_params:
      access_type: offline
      prompt: consent
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g := cfg.OAuthProviders["google"]
	if g.Flow != "loopback" {
		t.Errorf("Flow = %q, want loopback", g.Flow)
	}
	if g.AuthorizationEndpoint != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Errorf("AuthorizationEndpoint = %q", g.AuthorizationEndpoint)
	}
	if g.AuthorizeParams["access_type"] != "offline" || g.AuthorizeParams["prompt"] != "consent" {
		t.Errorf("AuthorizeParams = %v, want access_type=offline, prompt=consent", g.AuthorizeParams)
	}
}

func TestLoadFromPath_OAuthProviders_LoopbackFlow_MissingAuthorizationEndpointRejected(t *testing.T) {
	content := `
oauth_providers:
  google:
    token_endpoint: https://oauth2.googleapis.com/token
    client_id: google-client-id
    flow: loopback
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for a loopback flow with no authorization_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "authorization_endpoint") {
		t.Errorf("error %q does not mention authorization_endpoint", err.Error())
	}
}

func TestLoadFromPath_OAuthProviders_ManualFlow(t *testing.T) {
	content := `
oauth_providers:
  freee:
    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token
    client_id: freee-client-id
    client_secret_key: freee-oauth-client-secret
    flow: manual
    authorization_endpoint: https://accounts.secure.freee.co.jp/public_api/authorize
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OAuthProviders["freee"].Flow != "manual" {
		t.Errorf("Flow = %q, want manual", cfg.OAuthProviders["freee"].Flow)
	}
}

func TestLoadFromPath_OAuthProviders_UnrecognizedFlowRejected(t *testing.T) {
	content := `
oauth_providers:
  freee:
    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token
    client_id: cid
    flow: telepathy
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for an unrecognized flow, got nil")
	}
	if !strings.Contains(err.Error(), "unrecognized") {
		t.Errorf("error %q does not mention 'unrecognized'", err.Error())
	}
}

func TestLoadFromPath_OAuthProviders_AuthorizationEndpointMustBeHTTPS(t *testing.T) {
	content := `
oauth_providers:
  google:
    token_endpoint: https://oauth2.googleapis.com/token
    client_id: cid
    flow: loopback
    authorization_endpoint: http://accounts.google.com/o/oauth2/v2/auth
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for a non-https authorization_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error %q does not mention https", err.Error())
	}
}

// TestLoadFromPath_OAuthProviders_AuthorizeParamsReservedKeyRejected pins
// that an operator cannot use authorize_params to override a
// protocol-reserved parameter the login flow's own construction already
// sets (e.g. redirect_uri, state, client_id).
func TestLoadFromPath_OAuthProviders_AuthorizeParamsReservedKeyRejected(t *testing.T) {
	content := `
oauth_providers:
  google:
    token_endpoint: https://oauth2.googleapis.com/token
    client_id: cid
    flow: loopback
    authorization_endpoint: https://accounts.google.com/o/oauth2/v2/auth
    authorize_params:
      redirect_uri: https://evil.example.com/callback
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for authorize_params overriding redirect_uri, got nil")
	}
	if !strings.Contains(err.Error(), "redirect_uri") {
		t.Errorf("error %q does not mention redirect_uri", err.Error())
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
			TokenEndpoint:               "https://accounts.secure.freee.co.jp/public_api/token",
			ClientID:                    "freee-client-id",
			ClientSecretKey:             "freee-oauth-client-secret",
			Scopes:                      []string{"read", "write"},
			Flow:                        "manual",
			AuthorizationEndpoint:       "https://accounts.secure.freee.co.jp/public_api/authorize",
			DeviceAuthorizationEndpoint: "",
			AuthorizeParams:             map[string]string{"foo": "bar"},
			Grant:                       "authorization_code",
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
	if string(p.Flow) != "manual" {
		t.Errorf("Flow = %q, want manual", p.Flow)
	}
	if p.AuthorizationEndpoint != "https://accounts.secure.freee.co.jp/public_api/authorize" {
		t.Errorf("AuthorizationEndpoint = %q", p.AuthorizationEndpoint)
	}
	if p.AuthorizeParams["foo"] != "bar" {
		t.Errorf("AuthorizeParams = %v, want foo=bar", p.AuthorizeParams)
	}
	if string(p.Grant) != "authorization_code" {
		t.Errorf("Grant = %q, want authorization_code", p.Grant)
	}
}

// --- grant: client_credentials (docs/plans/api-gateway.md §6-補, PR4) ---

// TestLoadFromPath_OAuthProviders_GrantOptional pins that an existing
// pre-§6-補 config.yaml (no grant field at all) keeps loading unmodified,
// and is treated as authorization_code — the same backward-compatibility
// posture Flow's own optionality already has (TestLoadFromPath_
// OAuthProviders_FlowOptional, above).
func TestLoadFromPath_OAuthProviders_GrantOptional(t *testing.T) {
	content := `
oauth_providers:
  freee:
    token_endpoint: https://accounts.secure.freee.co.jp/public_api/token
    client_id: freee-client-id
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error (grant must be optional): %v", err)
	}
	if cfg.OAuthProviders["freee"].Grant != "" {
		t.Errorf("Grant = %q, want empty", cfg.OAuthProviders["freee"].Grant)
	}
}

// TestLoadFromPath_OAuthProviders_ClientCredentials_Valid pins the
// client_credentials config shape (docs/plans/api-gateway.md §6-補): no
// flow, no authorization_endpoint — just token_endpoint/client_id/
// client_secret_key/scopes/grant.
func TestLoadFromPath_OAuthProviders_ClientCredentials_Valid(t *testing.T) {
	content := `
oauth_providers:
  az:
    token_endpoint: https://login.microsoftonline.com/some-tenant-id/oauth2/v2.0/token
    client_id: sp-client-id
    client_secret_key: az-sp-client-secret
    scopes: [https://api.example.com/.default]
    grant: client_credentials
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	az := cfg.OAuthProviders["az"]
	if az.Grant != "client_credentials" {
		t.Errorf("Grant = %q, want client_credentials", az.Grant)
	}
	if az.ClientSecretKey != "az-sp-client-secret" {
		t.Errorf("ClientSecretKey = %q", az.ClientSecretKey)
	}
}

func TestLoadFromPath_OAuthProviders_UnrecognizedGrantRejected(t *testing.T) {
	content := `
oauth_providers:
  az:
    token_endpoint: https://login.microsoftonline.com/some-tenant-id/oauth2/v2.0/token
    client_id: sp-client-id
    client_secret_key: az-sp-client-secret
    grant: implicit
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for an unrecognized grant, got nil")
	}
	if !strings.Contains(err.Error(), "grant") {
		t.Errorf("error %q does not mention grant", err.Error())
	}
}

// TestLoadFromPath_OAuthProviders_ClientCredentials_MissingClientSecretKeyRejected
// pins docs/plans/api-gateway.md §6-補 / RFC 6749 §4.4.2: client_credentials
// requires a confidential client, so client_secret_key is required — unlike
// the default authorization_code grant, where it is optional (PKCE public
// clients).
func TestLoadFromPath_OAuthProviders_ClientCredentials_MissingClientSecretKeyRejected(t *testing.T) {
	content := `
oauth_providers:
  az:
    token_endpoint: https://login.microsoftonline.com/some-tenant-id/oauth2/v2.0/token
    client_id: sp-client-id
    grant: client_credentials
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for client_credentials with no client_secret_key, got nil")
	}
	if !strings.Contains(err.Error(), "client_secret_key") {
		t.Errorf("error %q does not mention client_secret_key", err.Error())
	}
}

// TestLoadFromPath_OAuthProviders_ClientCredentials_FlowRejected pins
// docs/plans/api-gateway.md §6-補's "grant と flow の排他は config load 時
// に拒否する" decision: grant and flow are orthogonal axes, but
// client_credentials has no login flow at all, so setting both must fail
// config load rather than being silently accepted and only failing later
// at `boid secret oauth login` time.
func TestLoadFromPath_OAuthProviders_ClientCredentials_FlowRejected(t *testing.T) {
	content := `
oauth_providers:
  az:
    token_endpoint: https://login.microsoftonline.com/some-tenant-id/oauth2/v2.0/token
    client_id: sp-client-id
    client_secret_key: az-sp-client-secret
    grant: client_credentials
    flow: manual
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for client_credentials with a flow set, got nil")
	}
	if !strings.Contains(err.Error(), "flow") {
		t.Errorf("error %q does not mention flow", err.Error())
	}
}
