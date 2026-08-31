package integrationpack

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/config"
)

// TestDesugarService_EndpointSecretKey_PropagatesToBaseURLSecretKey pins the
// D13 (docs/plans/api-gateway-credential-accounts.md) counterpart to
// TestDesugarService_Valid's literal-Endpoint case: an instance that sets
// endpoint_secret_key instead of endpoint desugars into an
// apigateway.ServiceConfig with BaseURL empty and BaseURLSecretKey carrying
// the key — the exact field apigateway.CredentialProvider.BaseURLFor
// already knows how to resolve (D12's runtime machinery, unmodified by
// D13 — see this package's doc.go / the D13 decision note for why no
// runtime code changed).
func TestDesugarService_EndpointSecretKey_PropagatesToBaseURLSecretKey(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:              "jira-cloud/jira-cloud@1.2.0",
		EndpointSecretKey: "JIRA_BASE_URL",
		Credentials:       map[string]string{"token": "JIRA_TOKEN"},
	}
	got, err := DesugarService("customer-jira", sc, bearerPack(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty (endpoint_secret_key path)", got.BaseURL)
	}
	if got.BaseURLSecretKey != "JIRA_BASE_URL" {
		t.Errorf("BaseURLSecretKey = %q, want %q (dropped by DesugarService)", got.BaseURLSecretKey, "JIRA_BASE_URL")
	}
}

// TestDesugarService_EndpointAndEndpointSecretKeyMutuallyExclusive pins the
// defensive re-check DesugarService performs for a hand-built
// config.ServiceConfig that skipped config.validateServiceConfig (this
// function's own doc comment, item 1) — mirroring every other "already
// checked once at config-load time, re-checked here" guard in this file.
func TestDesugarService_EndpointAndEndpointSecretKeyMutuallyExclusive(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:              "jira-cloud/jira-cloud@1.2.0",
		Endpoint:          "https://example.atlassian.net",
		EndpointSecretKey: "JIRA_BASE_URL",
		Credentials:       map[string]string{"token": "JIRA_TOKEN"},
	}
	if _, err := DesugarService("customer-jira", sc, bearerPack(t)); err == nil {
		t.Fatal("want error for endpoint + endpoint_secret_key both set, got nil")
	}
}

// TestDesugarService_EndpointSecretKey_NotConfigurableRejected mirrors
// TestDesugarService_EndpointNotConfigurableRejected for the secret-backed
// field: a profile declaring endpoint.configurable: false accepts neither
// endpoint nor endpoint_secret_key.
func TestDesugarService_EndpointSecretKey_NotConfigurableRejected(t *testing.T) {
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
		Uses:              "slack/slack@1.1.0",
		EndpointSecretKey: "SHOULD_NOT_BE_SETTABLE",
		Credentials:       map[string]string{"token": "SLACK_TOKEN"},
	}
	if _, err := DesugarService("my-slack", sc, packs); err == nil {
		t.Fatal("want error for endpoint_secret_key set on a non-configurable profile, got nil")
	}
}

// TestDesugarService_UsernameSecretKey_PropagatesToAuthUsernameSecretKey
// mirrors TestDesugarService_BasicInjectionUsernameFromInstance for the
// secret-backed field (D13): an instance that sets username_secret_key
// instead of username, on a profile whose basic slot declares
// usernameFrom: instance, desugars into apigateway.ServiceAuth with
// Username empty and UsernameSecretKey carrying the key — deferred to
// apigateway.CredentialProvider.resolveUsername at request time, the same
// way a free-form entry's auth.username_secret_key already is.
func TestDesugarService_UsernameSecretKey_PropagatesToAuthUsernameSecretKey(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:              "jira-cloud/jira-cloud@1.0.0",
		Endpoint:          "https://example.atlassian.net",
		UsernameSecretKey: "JIRA_USERNAME",
		Credentials:       map[string]string{"token": "JIRA_TOKEN"},
	}
	got, err := DesugarService("customer-jira", sc, jiraCloudBasicInstanceUsernamePack(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Auth.Username != "" {
		t.Errorf("Auth.Username = %q, want empty (username_secret_key path)", got.Auth.Username)
	}
	if got.Auth.UsernameSecretKey != "JIRA_USERNAME" {
		t.Errorf("Auth.UsernameSecretKey = %q, want %q (dropped by DesugarService)", got.Auth.UsernameSecretKey, "JIRA_USERNAME")
	}
}

// TestDesugarService_UsernameAndUsernameSecretKeyMutuallyExclusive mirrors
// TestDesugarService_EndpointAndEndpointSecretKeyMutuallyExclusive for the
// username pair.
func TestDesugarService_UsernameAndUsernameSecretKeyMutuallyExclusive(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:              "jira-cloud/jira-cloud@1.0.0",
		Endpoint:          "https://example.atlassian.net",
		Username:          "alice@example.com",
		UsernameSecretKey: "JIRA_USERNAME",
		Credentials:       map[string]string{"token": "JIRA_TOKEN"},
	}
	if _, err := DesugarService("customer-jira", sc, jiraCloudBasicInstanceUsernamePack(t)); err == nil {
		t.Fatal("want error for username + username_secret_key both set, got nil")
	}
}

// TestDesugarService_UsernameSecretKey_NotAcceptedOnNonBasicInjection
// mirrors TestDesugarService_NonBasicInjection_InstanceUsernameRejected for
// the secret-backed field: a bearer-injection profile has no username slot
// for username_secret_key to fill either.
func TestDesugarService_UsernameSecretKey_NotAcceptedOnNonBasicInjection(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:              "jira-cloud/jira-cloud@1.2.0",
		Endpoint:          "https://example.atlassian.net",
		UsernameSecretKey: "SHOULD_NOT_BE_SETTABLE",
		Credentials:       map[string]string{"token": "JIRA_TOKEN"},
	}
	if _, err := DesugarService("customer-jira", sc, bearerPack(t)); err == nil {
		t.Fatal("want error for username_secret_key set on a bearer-injection profile, got nil")
	}
}

// TestDesugarService_UsernameSecretKey_NotAcceptedOnFixedUsernameProfile
// mirrors TestDesugarService_BasicFixedUsernameProfile_InstanceUsernameRejected
// for the secret-backed field: a Pack-fixed-username basic profile (e.g.
// bitbucket-cloud) has no slot for an instance-supplied username_secret_key
// to fill either.
func TestDesugarService_UsernameSecretKey_NotAcceptedOnFixedUsernameProfile(t *testing.T) {
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
		Uses:              "bitbucket-cloud/bitbucket-cloud@1.3.0",
		Endpoint:          "https://api.bitbucket.org/2.0",
		UsernameSecretKey: "SHOULD_NOT_BE_SETTABLE",
		Credentials:       map[string]string{"token": "BB_TOKEN"},
	}
	if _, err := DesugarService("bitbucket-api", sc, packs); err == nil {
		t.Fatal("want error for username_secret_key set on a basic-injection profile with a Pack-fixed username, got nil")
	}
}

// TestDesugarService_D13_OnePackInstanceTwoNamespacesNoAccountNeeded is the
// D13 money test — the actual production motivation this feature shipped
// for (docs/plans/api-gateway-credential-accounts.md D13): jira-cloud
// instantiated exactly ONCE in config.yaml, resolving to a DIFFERENT
// upstream host and Basic-auth username depending on which NAMESPACE
// (workspace) a request comes from — with NO account (@) qualifier
// anywhere, unlike D12's account-qualified scenario
// (TestServer_JiraCloudScenario_OneServiceTwoAccountsDifferentHostAndUsername,
// internal/apigateway/server_test.go), which varies identity within ONE
// namespace via "@ubs"/"@aolani". This replaces the two-named-Pack-instance
// stopgap (jira-cloud / jira-cloud-ubs) production actually ran with before
// this PR. Deliberately exercises the FULL pipeline — DesugarService then a
// real apigateway.CredentialProvider — not just DesugarService's output in
// isolation, since the whole point of D13 is that NO runtime code changed:
// this is proof, not assertion, that reusing apigateway.ServiceConfig's
// existing D12 fields is sufficient.
func TestDesugarService_D13_OnePackInstanceTwoNamespacesNoAccountNeeded(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:              "jira-cloud/jira-cloud@1.0.0",
		EndpointSecretKey: "JIRA_BASE_URL",
		UsernameSecretKey: "JIRA_USERNAME",
		Credentials:       map[string]string{"token": "JIRA_API_TOKEN"},
	}
	desugared, err := DesugarService("jira-cloud", sc, jiraCloudBasicInstanceUsernamePack(t))
	if err != nil {
		t.Fatalf("DesugarService: %v", err)
	}

	values := map[string]string{
		"khi/JIRA_BASE_URL":      "https://aolani.atlassian.net",
		"khi/JIRA_USERNAME":      "naoki.nose@kameda-hi.co.jp",
		"khi/JIRA_API_TOKEN":     "tok-khi",
		"default/JIRA_BASE_URL":  "https://urban-b.atlassian.net",
		"default/JIRA_USERNAME":  "nose@urban-b.com",
		"default/JIRA_API_TOKEN": "tok-default",
	}
	resolver := func(namespace, key string) (string, error) {
		v, ok := values[namespace+"/"+key]
		if !ok {
			return "", fmt.Errorf("no value for namespace=%q key=%q", namespace, key)
		}
		return v, nil
	}
	creds := apigateway.NewCredentialProvider([]apigateway.ServiceConfig{desugared}, resolver)

	khiURL, err := creds.BaseURLFor("khi", "jira-cloud", "")
	if err != nil {
		t.Fatalf("BaseURLFor(khi): %v", err)
	}
	if khiURL.String() != "https://aolani.atlassian.net" {
		t.Errorf("BaseURLFor(khi) = %q, want %q", khiURL.String(), "https://aolani.atlassian.net")
	}

	defaultURL, err := creds.BaseURLFor("default", "jira-cloud", "")
	if err != nil {
		t.Fatalf("BaseURLFor(default): %v", err)
	}
	if defaultURL.String() != "https://urban-b.atlassian.net" {
		t.Errorf("BaseURLFor(default) = %q, want %q", defaultURL.String(), "https://urban-b.atlassian.net")
	}
	if khiURL.String() == defaultURL.String() {
		t.Fatal("test setup bug: khi and default resolved to the same URL")
	}

	wantAuthHeader := func(user, pass string) string {
		r, _ := http.NewRequest(http.MethodGet, "https://example.invalid", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	khiReq, _ := http.NewRequest(http.MethodGet, "https://example.invalid/rest/api/3/myself", nil)
	if err := creds.Inject(khiReq, "khi", "jira-cloud", ""); err != nil {
		t.Fatalf("Inject(khi): %v", err)
	}
	if want := wantAuthHeader("naoki.nose@kameda-hi.co.jp", "tok-khi"); khiReq.Header.Get("Authorization") != want {
		t.Errorf("khi Authorization = %q, want %q", khiReq.Header.Get("Authorization"), want)
	}

	defaultReq, _ := http.NewRequest(http.MethodGet, "https://example.invalid/rest/api/3/myself", nil)
	if err := creds.Inject(defaultReq, "default", "jira-cloud", ""); err != nil {
		t.Fatalf("Inject(default): %v", err)
	}
	if want := wantAuthHeader("nose@urban-b.com", "tok-default"); defaultReq.Header.Get("Authorization") != want {
		t.Errorf("default Authorization = %q, want %q", defaultReq.Header.Get("Authorization"), want)
	}
}

// TestDesugarService_D13_LiteralEndpointAndUsernameStillWorkUnchanged is the
// backward-compatibility pin: an existing Pack-instance config using the
// pre-D13 literal endpoint/username fields (no *_secret_key anywhere)
// resolves byte-identically to before — D13 is purely additive.
func TestDesugarService_D13_LiteralEndpointAndUsernameStillWorkUnchanged(t *testing.T) {
	sc := config.ServiceConfig{
		Uses:        "jira-cloud/jira-cloud@1.0.0",
		Endpoint:    "https://example.atlassian.net",
		Username:    "alice@example.com",
		Credentials: map[string]string{"token": "JIRA_TOKEN"},
	}
	got, err := DesugarService("customer-jira", sc, jiraCloudBasicInstanceUsernamePack(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := apigateway.ServiceConfig{
		Name:    "customer-jira",
		BaseURL: "https://example.atlassian.net",
		Auth: apigateway.ServiceAuth{
			Kind:      apigateway.AuthBasic,
			SecretKey: "JIRA_TOKEN",
			Username:  "alice@example.com",
		},
	}
	if got != want {
		t.Errorf("DesugarService() = %+v, want %+v", got, want)
	}
}
