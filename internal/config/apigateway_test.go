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

// TestLoadFromPath_Services_WhitespacePaddedNameRejected pins a codex review
// finding: orchestrator.ResolveEnabledServices trims every service name it
// resolves, but the service registry built from this config is keyed
// verbatim by the services.<name> map key — a whitespace-padded name would
// validate cleanly yet never resolve for any workspace (KnowsService always
// 502s), a silent, hard-to-diagnose misconfiguration. Reject it outright at
// config-load time instead.
func TestLoadFromPath_Services_WhitespacePaddedNameRejected(t *testing.T) {
	content := "services:\n  \" myapp\":\n    base_url: https://myapp.example.com\n    auth: { kind: bearer, secret_key: k }\n"
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for a whitespace-padded service name, got nil")
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("error should mention whitespace, got: %v", err)
	}
}

// TestLoadFromPath_Services_EmptyNameRejected pins a codex review finding
// (round 7): an empty service name (`services: {"": ...}`) passes
// TrimSpace's whitespace check (TrimSpace("") == "") and every other check,
// yet apigateway.parsePath's route segment for a service name can never be
// empty — a service declared this way would validate cleanly at
// config-load time but be permanently unreachable from any sandbox request.
func TestLoadFromPath_Services_EmptyNameRejected(t *testing.T) {
	content := "services:\n  \"\":\n    base_url: https://myapp.example.com\n    auth: { kind: bearer, secret_key: k }\n"
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for an empty service name, got nil")
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

// TestLoadFromPath_Services_BaseURLWithQueryOrFragmentRejected pins a codex
// review round-2 finding: apigateway.Server always forwards the inbound
// request's own RawQuery, never merging in anything from base_url, so a
// base_url with a query string (or fragment, which HTTP never sends at
// all) would have silently vanished on every request with no indication
// why. Reject it outright at config-load time instead.
func TestLoadFromPath_Services_BaseURLWithQueryOrFragmentRejected(t *testing.T) {
	cases := []string{
		"https://myapp.example.com/api?tenant=x",
		"https://myapp.example.com/api#section",
	}
	for _, baseURL := range cases {
		content := "services:\n  myapp:\n    base_url: " + baseURL + "\n    auth: { kind: bearer, secret_key: k }\n"
		err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content)
		if err == nil {
			t.Errorf("base_url %q: want error for query string/fragment, got nil", baseURL)
		}
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

// TestLoadFromPath_Services_BasicUsernameWithColonRejected pins a codex
// review finding (round 6): RFC 7617 §2 builds a Basic credential as
// "username:password" and splits on the FIRST colon, so a username itself
// containing one changes what the upstream parses as each half.
// net/http.Request.SetBasicAuth (what CredentialProvider.Inject calls) does
// not validate this on its own — it just base64-encodes verbatim — so this
// must be caught at config-load time instead of surfacing as a confusing
// upstream 401.
func TestLoadFromPath_Services_BasicUsernameWithColonRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: basic, username: "a:b", secret_key: k }
`
	if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
		t.Fatal("want error for a basic-auth username containing \":\", got nil")
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

// TestLoadFromPath_Services_ReservedHeaderNameRejected pins a codex review
// finding (round 5): "Host" is a syntactically valid HTTP header field name
// (passes isValidHTTPHeaderFieldName's RFC 7230 token check) but Go's
// net/http never sends an outgoing request's Header["Host"] entry on the
// wire — the actual Host header comes from Request.Host/Request.URL.Host
// instead. auth.kind: header with header: Host would therefore have
// CredentialProvider.Inject report success while the secret reaches the
// upstream on no channel at all: a silent, reported-success auth bypass.
// The other names here are net/http's hop-by-hop set plus Content-Length,
// all managed by the Transport itself for an outgoing request.
func TestLoadFromPath_Services_ReservedHeaderNameRejected(t *testing.T) {
	cases := []string{"Host", "host", "Content-Length", "Transfer-Encoding", "Connection", "Upgrade"}
	for _, header := range cases {
		content := "services:\n  myapp:\n    base_url: https://myapp.example.com\n    auth:\n      kind: header\n      header: \"" + header + "\"\n      secret_key: k\n"
		if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
			t.Errorf("header %q: want error for reserved/transport header name, got nil", header)
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

// TestLoadFromPath_Services_HTTPBaseURLWithoutAllowInsecureRejected pins a
// codex review finding (round 4, escalated from a round-2 warn-only
// version of this check): a plain-http base_url means the injected
// credential crosses the network to the upstream in cleartext, and a bare
// log warning is not fail-closed — an operator who never reads boid.log has
// no signal at all. Without an explicit `allow_insecure: true` on the
// service, this must be a hard config-load error.
func TestLoadFromPath_Services_HTTPBaseURLWithoutAllowInsecureRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: http://myapp-internal.example.com
    auth: { kind: bearer, secret_key: k }
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for http base_url without allow_insecure, got nil")
	}
	if !strings.Contains(err.Error(), "allow_insecure") {
		t.Errorf("error should mention allow_insecure as the remedy, got: %v", err)
	}
}

// TestLoadFromPath_Services_HTTPBaseURLWithAllowInsecureWarnsButDoesNotFail
// is the companion positive case: PR1's own primary stated use case
// (internal test/ops APIs) often has no TLS yet in a staging environment,
// so plaintext must still be SUPPORTED — just as an explicit, config-visible
// opt-in rather than a silently-accepted default. See
// validateServiceConfig's own doc comment for the full reasoning.
func TestLoadFromPath_Services_HTTPBaseURLWithAllowInsecureWarnsButDoesNotFail(t *testing.T) {
	buf := captureSlog(t)
	content := `
services:
  myapp:
    base_url: http://myapp-internal.example.com
    allow_insecure: true
    auth: { kind: bearer, secret_key: k }
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error (http base_url with allow_insecure: true must be accepted, only warned about): %v", err)
	}
	if cfg.Services["myapp"].BaseURL != "http://myapp-internal.example.com" {
		t.Errorf("Services[myapp].BaseURL = %q", cfg.Services["myapp"].BaseURL)
	}
	if !cfg.Services["myapp"].AllowInsecure {
		t.Error("Services[myapp].AllowInsecure = false, want true")
	}
	if !strings.Contains(buf.String(), "myapp") || !strings.Contains(buf.String(), "cleartext") {
		t.Errorf("expected a warning naming the service and the cleartext risk, log = %q", buf.String())
	}
}

// TestLoadFromPath_Services_AllowInsecureDoesNotPermitArbitraryScheme pins a
// codex review finding (round 6): allow_insecure was meant as a plaintext-
// HTTP escape hatch only, but the scheme check it participated in accepted
// ANY non-https scheme once set — "ftp"/"ws"/anything else validated
// cleanly yet the gateway's fixed *http.Transport (server.go's NewServer)
// only ever speaks http/https, so every such request would fail at 502
// regardless. This must be a config-load error, not a request-time surprise.
func TestLoadFromPath_Services_AllowInsecureDoesNotPermitArbitraryScheme(t *testing.T) {
	cases := []string{"ftp", "ws", "wss", "gopher"}
	for _, scheme := range cases {
		content := "services:\n  myapp:\n    base_url: " + scheme + "://myapp-internal.example.com\n    allow_insecure: true\n    auth: { kind: bearer, secret_key: k }\n"
		if err := writeAndLoad(t, filepath.Join(t.TempDir(), "config.yaml"), content); err == nil {
			t.Errorf("scheme %q: want error even with allow_insecure: true (gateway only speaks http/https), got nil", scheme)
		}
	}
}

// TestLoadFromPath_Services_UsesValid pins the "uses:" instance shape
// (docs/plans/signal-driven-review.md §7.2): a services.<name> entry may
// reference an installed Integration Pack's service profile instead of
// writing base_url/auth by hand. Uses/Endpoint/Credentials parse verbatim —
// this package cannot resolve the reference itself (it has no access to the
// installed Pack registry; internal/integrationpack does that against the
// loaded Packs) — see APIGatewayServices' own doc comment for why a uses:
// entry is excluded from its output.
func TestLoadFromPath_Services_UsesValid(t *testing.T) {
	content := `
services:
  customer-jira:
    uses: jira-cloud/jira-cloud@1.2.0
    endpoint: https://example.atlassian.net
    credentials:
      token: JIRA_TOKEN
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sc := cfg.Services["customer-jira"]
	if sc.Uses != "jira-cloud/jira-cloud@1.2.0" {
		t.Errorf("Uses = %q", sc.Uses)
	}
	if sc.Endpoint != "https://example.atlassian.net" {
		t.Errorf("Endpoint = %q", sc.Endpoint)
	}
	if sc.Credentials["token"] != "JIRA_TOKEN" {
		t.Errorf("Credentials[token] = %q, want JIRA_TOKEN", sc.Credentials["token"])
	}
}

// TestLoadFromPath_Services_UsesExclusiveWithBaseURLRejected pins the
// documented exclusivity (docs/plans/signal-ingest-detailed-design.md §6.1:
// "uses 指定時は既存の base_url/auth と排他"): a services.<name> entry
// cannot set both — the base_url would come from the resolved Pack service
// profile, so a hand-written one alongside it is an unresolvable
// contradiction, not a harmless override.
func TestLoadFromPath_Services_UsesExclusiveWithBaseURLRejected(t *testing.T) {
	content := `
services:
  customer-jira:
    uses: jira-cloud/jira-cloud@1.2.0
    base_url: https://example.atlassian.net
    credentials:
      token: JIRA_TOKEN
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for uses: + base_url:, got nil")
	}
	if !strings.Contains(err.Error(), "uses") || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("error should mention both uses and base_url, got: %v", err)
	}
}

// TestLoadFromPath_Services_UsesExclusiveWithAuthRejected is
// UsesExclusiveWithBaseURLRejected's auth: counterpart — credential
// injection for a uses: entry comes from the resolved profile's declared
// slot (bound via credentials:), not a hand-written auth: block.
func TestLoadFromPath_Services_UsesExclusiveWithAuthRejected(t *testing.T) {
	content := `
services:
  customer-jira:
    uses: jira-cloud/jira-cloud@1.2.0
    auth: { kind: bearer, secret_key: JIRA_TOKEN }
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for uses: + auth:, got nil")
	}
	if !strings.Contains(err.Error(), "uses") || !strings.Contains(err.Error(), "auth") {
		t.Errorf("error should mention both uses and auth, got: %v", err)
	}
}

// TestLoadFromPath_Services_UsesMalformedRejected pins the "<pack>/<profile>@<version>"
// syntax (docs/plans/signal-driven-review.md §7.2) at config-load time —
// this package cannot check the reference resolves to an installed Pack
// (see TestLoadFromPath_Services_UsesValid's own doc comment), but a
// structurally malformed reference is a config-authoring mistake this
// package CAN and should catch eagerly, the same "fail loud with the
// offending name" posture every other leaf in this file has.
func TestLoadFromPath_Services_UsesMalformedRejected(t *testing.T) {
	cases := []string{
		"jira-cloud",             // no "/" and no "@" at all
		"jira-cloud@1.2.0",       // no "/" — missing profile
		"jira-cloud/jira-cloud",  // no "@" — missing version
		"/jira-cloud@1.2.0",      // empty pack name
		"jira-cloud/@1.2.0",      // empty profile name
		"jira-cloud/jira-cloud@", // empty version
	}
	for _, uses := range cases {
		content := "services:\n  myapp:\n    uses: \"" + uses + "\"\n    credentials: { token: T }\n"
		if _, err := loadFromPath(writeConfigFile(t, content)); err == nil {
			t.Errorf("uses %q: want error for malformed uses: reference, got nil", uses)
		}
	}
}

// TestLoadFromPath_Services_EndpointWithoutUsesRejected pins that endpoint:
// only makes sense alongside uses: (it fills in a Pack service profile's
// endpoint.configurable slot — signal-driven-review.md §7.2) — a free-form
// base_url/auth entry has no such slot to fill, so setting it is very
// likely a stray copy-paste rather than anything meaningful.
func TestLoadFromPath_Services_EndpointWithoutUsesRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    endpoint: https://should-not-be-here.example.com
    auth: { kind: bearer, secret_key: k }
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for endpoint: without uses:, got nil")
	}
	if !strings.Contains(err.Error(), "endpoint") || !strings.Contains(err.Error(), "uses") {
		t.Errorf("error should mention both endpoint and uses, got: %v", err)
	}
}

// TestLoadFromPath_Services_CredentialsWithoutUsesRejected is
// EndpointWithoutUsesRejected's credentials: counterpart — credentials:
// binds a Pack service profile's declared slot names, which only exist once
// uses: resolves to one.
func TestLoadFromPath_Services_CredentialsWithoutUsesRejected(t *testing.T) {
	content := `
services:
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: bearer, secret_key: k }
    credentials:
      token: SOME_KEY
`
	_, err := loadFromPath(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("want error for credentials: without uses:, got nil")
	}
	if !strings.Contains(err.Error(), "credentials") || !strings.Contains(err.Error(), "uses") {
		t.Errorf("error should mention both credentials and uses, got: %v", err)
	}
}

// TestAPIGatewayServices_ExcludesUsesEntries pins that APIGatewayServices()
// — the flat []apigateway.ServiceConfig list built from ONLY this package's
// own knowledge (base_url/auth) — never emits a broken entry for a uses:
// service (which has no BaseURL/Auth of its own to convert: those come from
// the resolved Pack profile, which this package cannot reach). Combining
// this method's output with the Pack-resolved uses: entries is
// internal/integrationpack.ResolveServices' job (internal/server/wire.go's
// gateway wiring point calls that instead of this method alone, once
// wired) — see this method's own doc comment.
func TestAPIGatewayServices_ExcludesUsesEntries(t *testing.T) {
	cfg := &Config{Services: map[string]ServiceConfig{
		"plain": {BaseURL: "https://plain.example.com", Auth: ServiceAuthConfig{Kind: "bearer", SecretKey: "k"}},
		"packed": {
			Uses:        "jira-cloud/jira-cloud@1.2.0",
			Endpoint:    "https://example.atlassian.net",
			Credentials: map[string]string{"token": "JIRA_TOKEN"},
		},
	}}
	services := cfg.APIGatewayServices()
	if len(services) != 1 {
		t.Fatalf("len(APIGatewayServices()) = %d, want 1 (the uses: entry must be excluded)", len(services))
	}
	if services[0].Name != "plain" {
		t.Errorf("APIGatewayServices()[0].Name = %q, want %q", services[0].Name, "plain")
	}
}

// TestParseUsesReference pins ParseUsesReference's exact splitting rule —
// the single source of truth both validateServiceConfig (syntax check, this
// package) and internal/integrationpack (semantic resolution against the
// loaded Pack registry) share, so the two can never drift apart on an edge
// case.
func TestParseUsesReference(t *testing.T) {
	cases := []struct {
		uses                           string
		wantPack, wantProfile, wantVer string
		wantErr                        bool
	}{
		{uses: "jira-cloud/jira-cloud@1.2.0", wantPack: "jira-cloud", wantProfile: "jira-cloud", wantVer: "1.2.0"},
		{uses: "slack/slack@1.1.0", wantPack: "slack", wantProfile: "slack", wantVer: "1.1.0"},
		{uses: "jira-cloud", wantErr: true},
		{uses: "jira-cloud@1.2.0", wantErr: true},
		{uses: "jira-cloud/jira-cloud", wantErr: true},
		{uses: "/jira-cloud@1.2.0", wantErr: true},
		{uses: "jira-cloud/@1.2.0", wantErr: true},
		{uses: "jira-cloud/jira-cloud@", wantErr: true},
		{uses: "", wantErr: true},
	}
	for _, tc := range cases {
		pack, profile, ver, err := ParseUsesReference(tc.uses)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseUsesReference(%q): want error, got nil", tc.uses)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseUsesReference(%q): unexpected error: %v", tc.uses, err)
			continue
		}
		if pack != tc.wantPack || profile != tc.wantProfile || ver != tc.wantVer {
			t.Errorf("ParseUsesReference(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tc.uses, pack, profile, ver, tc.wantPack, tc.wantProfile, tc.wantVer)
		}
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

// TestLoadFromPath_Services_AllowReadOnlyWrite pins the 2026-08-14 addition
// (docs/plans/api-gateway.md §論点): allow_readonly_write parses into
// ServiceConfig.AllowReadOnlyWrite and propagates through
// APIGatewayServices to the apigateway.ServiceConfig the gateway actually
// gates requests against — a gap here would make the config-level opt-in a
// no-op at the gateway.
func TestLoadFromPath_Services_AllowReadOnlyWrite(t *testing.T) {
	content := `
services:
  slack:
    base_url: https://slack.example.com
    allow_readonly_write: true
    auth: { kind: bearer, secret_key: k }
  myapp:
    base_url: https://myapp.example.com
    auth: { kind: bearer, secret_key: k }
`
	cfg, err := loadFromPath(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Services["slack"].AllowReadOnlyWrite {
		t.Error("Services[slack].AllowReadOnlyWrite = false, want true")
	}
	if cfg.Services["myapp"].AllowReadOnlyWrite {
		t.Error("Services[myapp].AllowReadOnlyWrite = true, want false (defaults to fail-closed)")
	}

	services := cfg.APIGatewayServices()
	byName := make(map[string]bool, len(services))
	for _, s := range services {
		byName[s.Name] = s.AllowReadOnlyWrite
	}
	if !byName["slack"] {
		t.Error("APIGatewayServices()[slack].AllowReadOnlyWrite = false, want true (config value was dropped in translation)")
	}
	if byName["myapp"] {
		t.Error("APIGatewayServices()[myapp].AllowReadOnlyWrite = true, want false")
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
