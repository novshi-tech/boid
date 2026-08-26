package integrationpack

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/config"
)

// bearerPack returns a single loaded *Pack named "jira-cloud"@"1.2.0" with
// one service profile ("jira-cloud", endpoint.configurable: true) whose
// sole credential slot ("token") injects as a bearer token — the exact
// shape docs/plans/signal-driven-review.md §6.3/§7.2 uses as its running
// example.
func bearerPack(t *testing.T) []*Pack {
	t.Helper()
	m, err := ParseManifest([]byte(jiraManifest("jira-cloud", "1.2.0")))
	if err != nil {
		t.Fatal(err)
	}
	return []*Pack{{Name: m.Metadata.Name, Version: m.Metadata.Version, Dir: "/opt/boid/integrations/jira-cloud/1.2.0", Manifest: *m}}
}

// TestDesugarService_Valid pins the "脱糖" mapping itself (docs/plans/
// signal-ingest-detailed-design.md §6.2 item 2): a uses:-based instance
// resolves to an apigateway.ServiceConfig whose BaseURL comes from
// Endpoint and whose Auth comes from the profile's credential slot +
// the instance's bound SecretStore key.
func TestDesugarService_Valid(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:        "jira-cloud/jira-cloud@1.2.0",
		Endpoint:    "https://example.atlassian.net",
		Credentials: map[string]string{"token": "JIRA_TOKEN"},
	}
	got, err := DesugarService("customer-jira", sc, bearerPack(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := apigateway.ServiceConfig{
		Name:    "customer-jira",
		BaseURL: "https://example.atlassian.net",
		Auth: apigateway.ServiceAuth{
			Kind:      apigateway.AuthBearer,
			SecretKey: "JIRA_TOKEN",
		},
	}
	if got != want {
		t.Errorf("DesugarService() = %+v, want %+v", got, want)
	}
}

// TestDesugarService_PropagatesAllowReadOnlyWrite pins a review finding
// (F2, MEDIUM): config.ServiceConfig.AllowReadOnlyWrite lives on the exact
// same struct a uses: entry uses — it was silently dropped because the
// uses: branch of validateServiceConfig returns before ever reaching it,
// and DesugarService's own returned apigateway.ServiceConfig never read it
// either. An operator setting allow_readonly_write: true on a uses: entry
// must see it actually reach the gateway's read-only gate, the same as a
// free-form base_url/auth entry already does (config.APIGatewayServices()).
func TestDesugarService_PropagatesAllowReadOnlyWrite(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:               "jira-cloud/jira-cloud@1.2.0",
		Endpoint:           "https://example.atlassian.net",
		Credentials:        map[string]string{"token": "JIRA_TOKEN"},
		AllowReadOnlyWrite: true,
	}
	got, err := DesugarService("customer-jira", sc, bearerPack(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.AllowReadOnlyWrite {
		t.Error("AllowReadOnlyWrite = false, want true (config value was dropped by DesugarService)")
	}
}

// TestDesugarService_PackNotInstalled pins Q19's "pack 不在" startup-error
// case: a uses: reference naming a pack/version that isn't among the loaded
// Packs is a config error, not a nil/zero-value fallback.
func TestDesugarService_PackNotInstalled(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:        "jira-cloud/jira-cloud@9.9.9",
		Endpoint:    "https://example.atlassian.net",
		Credentials: map[string]string{"token": "JIRA_TOKEN"},
	}
	_, err := DesugarService("customer-jira", sc, bearerPack(t))
	if err == nil {
		t.Fatal("want error for a pack/version that is not installed, got nil")
	}
}

// TestDesugarService_UnknownProfileRejected pins that the profile name half
// of uses: must also resolve within the found pack.
func TestDesugarService_UnknownProfileRejected(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:        "jira-cloud/nonexistent-profile@1.2.0",
		Endpoint:    "https://example.atlassian.net",
		Credentials: map[string]string{"token": "JIRA_TOKEN"},
	}
	_, err := DesugarService("customer-jira", sc, bearerPack(t))
	if err == nil {
		t.Fatal("want error for an unknown service profile, got nil")
	}
}

// TestDesugarService_UnboundCredentialSlotRejected pins Q19's "slot 未
// bind": the instance must bind every credential slot the profile
// declares.
func TestDesugarService_UnboundCredentialSlotRejected(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:     "jira-cloud/jira-cloud@1.2.0",
		Endpoint: "https://example.atlassian.net",
		// no credentials at all — "token" slot left unbound.
	}
	_, err := DesugarService("customer-jira", sc, bearerPack(t))
	if err == nil {
		t.Fatal("want error for an unbound credential slot, got nil")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should name the unbound slot, got: %v", err)
	}
}

// TestDesugarService_UnknownCredentialSlotRejected pins Q19's "未知 slot":
// an instance credentials: key that does not name any slot the profile
// declares (a likely typo) is rejected, not silently ignored.
func TestDesugarService_UnknownCredentialSlotRejected(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:     "jira-cloud/jira-cloud@1.2.0",
		Endpoint: "https://example.atlassian.net",
		Credentials: map[string]string{
			"token":   "JIRA_TOKEN",
			"typo_ed": "SOMETHING",
		},
	}
	_, err := DesugarService("customer-jira", sc, bearerPack(t))
	if err == nil {
		t.Fatal("want error for an unknown credential slot key, got nil")
	}
	if !strings.Contains(err.Error(), "typo_ed") {
		t.Errorf("error should name the unknown slot key, got: %v", err)
	}
}

// TestDesugarService_MissingRequiredEndpointRejected pins Q19's "endpoint
// 要求違反" (missing-value direction): the profile declares
// endpoint.configurable: true, so the instance must supply one.
func TestDesugarService_MissingRequiredEndpointRejected(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:        "jira-cloud/jira-cloud@1.2.0",
		Credentials: map[string]string{"token": "JIRA_TOKEN"},
		// no Endpoint.
	}
	_, err := DesugarService("customer-jira", sc, bearerPack(t))
	if err == nil {
		t.Fatal("want error for a missing required endpoint, got nil")
	}
}

// TestDesugarService_EndpointNotConfigurableRejected pins Q19's "endpoint
// 要求違反" (the other direction): a profile declaring endpoint.configurable:
// false does not accept an instance-supplied endpoint value at all in v0
// (there is no Pack-declared default-endpoint mechanism yet — see
// EndpointSpec's own doc comment).
func TestDesugarService_EndpointNotConfigurableRejected(t *testing.T) {
	m, err := ParseManifest([]byte(`
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: slack, version: '1.1.0'}
serviceProfiles:
  - name: slack
    credentials:
      - {name: token, injection: bearer}
`))
	if err != nil {
		t.Fatal(err)
	}
	packs := []*Pack{{Name: m.Metadata.Name, Version: m.Metadata.Version, Dir: "/x", Manifest: *m}}
	sc := config.ServiceConfig{
		Uses:        "slack/slack@1.1.0",
		Endpoint:    "https://should-not-be-settable.example.com",
		Credentials: map[string]string{"token": "SLACK_TOKEN"},
	}
	if _, err := DesugarService("my-slack", sc, packs); err == nil {
		t.Fatal("want error for endpoint set on a non-configurable profile, got nil")
	}
}

// TestDesugarService_HeaderInjection pins the header/query/basic
// desugaring shapes (not just bearer) — the credential slot's own
// Header/Query/Username fields flow straight into apigateway.ServiceAuth.
func TestDesugarService_HeaderInjection(t *testing.T) {
	m, err := ParseManifest([]byte(`
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: myapp, version: '1.0.0'}
serviceProfiles:
  - name: myapp
    endpoint: {configurable: true}
    credentials:
      - {name: token, injection: header, header: X-Api-Key}
`))
	if err != nil {
		t.Fatal(err)
	}
	packs := []*Pack{{Name: m.Metadata.Name, Version: m.Metadata.Version, Dir: "/x", Manifest: *m}}
	sc := config.ServiceConfig{
		Uses:        "myapp/myapp@1.0.0",
		Endpoint:    "https://myapp.example.com",
		Credentials: map[string]string{"token": "MYAPP_KEY"},
	}
	got, err := DesugarService("myapp-instance", sc, packs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Auth.Kind != apigateway.AuthHeader || got.Auth.Header != "X-Api-Key" || got.Auth.SecretKey != "MYAPP_KEY" {
		t.Errorf("Auth = %+v", got.Auth)
	}
}

// TestDesugarService_BasicInjection mirrors HeaderInjection for
// injection: basic, where Username is a Pack-declared constant (e.g.
// Bitbucket's "x-token-auth" convention) rather than an instance value —
// endpoint.configurable: true here only to isolate the injection-mapping
// behavior under test from EndpointNotConfigurableRejected's separate
// concern.
func TestDesugarService_BasicInjection(t *testing.T) {
	m, err := ParseManifest([]byte(`
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: bitbucket-cloud, version: '1.3.0'}
serviceProfiles:
  - name: bitbucket-cloud
    endpoint: {configurable: true}
    credentials:
      - {name: token, injection: basic, username: x-bitbucket-api-token-auth}
`))
	if err != nil {
		t.Fatal(err)
	}
	packs := []*Pack{{Name: m.Metadata.Name, Version: m.Metadata.Version, Dir: "/x", Manifest: *m}}
	sc := config.ServiceConfig{
		Uses:        "bitbucket-cloud/bitbucket-cloud@1.3.0",
		Endpoint:    "https://api.bitbucket.org/2.0",
		Credentials: map[string]string{"token": "BB_TOKEN"},
	}
	got, err := DesugarService("bitbucket-api", sc, packs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Auth.Kind != apigateway.AuthBasic || got.Auth.Username != "x-bitbucket-api-token-auth" || got.Auth.SecretKey != "BB_TOKEN" {
		t.Errorf("Auth = %+v", got.Auth)
	}
}

// TestDesugarService_QueryInjection mirrors HeaderInjection for
// injection: query.
func TestDesugarService_QueryInjection(t *testing.T) {
	m, err := ParseManifest([]byte(`
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: myapp, version: '1.0.0'}
serviceProfiles:
  - name: myapp
    endpoint: {configurable: true}
    credentials:
      - {name: token, injection: query, query: api_key}
`))
	if err != nil {
		t.Fatal(err)
	}
	packs := []*Pack{{Name: m.Metadata.Name, Version: m.Metadata.Version, Dir: "/x", Manifest: *m}}
	sc := config.ServiceConfig{
		Uses:        "myapp/myapp@1.0.0",
		Endpoint:    "https://myapp.example.com",
		Credentials: map[string]string{"token": "MYAPP_KEY"},
	}
	got, err := DesugarService("myapp-instance", sc, packs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Auth.Kind != apigateway.AuthQuery || got.Auth.Query != "api_key" || got.Auth.SecretKey != "MYAPP_KEY" {
		t.Errorf("Auth = %+v", got.Auth)
	}
}

// TestResolveServices combines a free-form services entry (passthrough via
// cfg.APIGatewayServices()) with a uses: entry (desugared against packs)
// into the single flat list internal/server/wire.go's gateway wiring point
// should call in place of cfg.APIGatewayServices() alone, once wired.
func TestResolveServices(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.ServiceConfig{
		"plain": {BaseURL: "https://plain.example.com", Auth: config.ServiceAuthConfig{Kind: "bearer", SecretKey: "k"}},
		"customer-jira": {
			Uses:        "jira-cloud/jira-cloud@1.2.0",
			Endpoint:    "https://example.atlassian.net",
			Credentials: map[string]string{"token": "JIRA_TOKEN"},
		},
	}}
	services, err := ResolveServices(cfg, bearerPack(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("len(services) = %d, want 2", len(services))
	}
	byName := make(map[string]apigateway.ServiceConfig, len(services))
	for _, s := range services {
		byName[s.Name] = s
	}
	if byName["plain"].BaseURL != "https://plain.example.com" {
		t.Errorf("plain.BaseURL = %q", byName["plain"].BaseURL)
	}
	if byName["customer-jira"].BaseURL != "https://example.atlassian.net" {
		t.Errorf("customer-jira.BaseURL = %q", byName["customer-jira"].BaseURL)
	}
	if byName["customer-jira"].Auth.SecretKey != "JIRA_TOKEN" {
		t.Errorf("customer-jira.Auth.SecretKey = %q", byName["customer-jira"].Auth.SecretKey)
	}
}

// TestResolveServices_UnresolvableUsesPropagatesError pins that a
// uses:-based entry which fails to resolve (uninstalled Pack) fails
// ResolveServices as a whole, rather than being silently dropped the way
// APIGatewayServices() drops an entry that fails its own (unrelated)
// validation — desugaring failure is a config/installation error, not a
// per-entry warning.
func TestResolveServices_UnresolvableUsesPropagatesError(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.ServiceConfig{
		"customer-jira": {
			Uses:        "jira-cloud/jira-cloud@9.9.9",
			Endpoint:    "https://example.atlassian.net",
			Credentials: map[string]string{"token": "JIRA_TOKEN"},
		},
	}}
	_, err := ResolveServices(cfg, bearerPack(t))
	if err == nil {
		t.Fatal("want error for an unresolvable uses: reference, got nil")
	}
}
