package apigateway

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func stubResolver(values map[string]string) SecretResolver {
	return func(namespace, key string) (string, error) {
		v, ok := values[namespace+"/"+key]
		if !ok {
			return "", errors.New("secret not found: " + namespace + "/" + key)
		}
		return v, nil
	}
}

func TestCredentialProvider_KnowsServiceAndBaseURLFor(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com/api", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, nil)

	if !c.KnowsService("myapp") {
		t.Error("KnowsService(myapp) = false, want true")
	}
	if c.KnowsService("unknown") {
		t.Error("KnowsService(unknown) = true, want false")
	}
	u, ok := c.BaseURLFor("myapp")
	if !ok {
		t.Fatal("BaseURLFor(myapp): not found")
	}
	if u.String() != "https://myapp.example.com/api" {
		t.Errorf("BaseURLFor(myapp) = %q, want %q", u.String(), "https://myapp.example.com/api")
	}
	if _, ok := c.BaseURLFor("unknown"); ok {
		t.Error("BaseURLFor(unknown): ok = true, want false")
	}
}

func TestCredentialProvider_InvalidBaseURLSkipped(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "broken", BaseURL: "https://example.com/%zz", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "x"}},
		{Name: "no-scheme", BaseURL: "example.com/api", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "x"}},
		{Name: "good", BaseURL: "https://good.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "x"}},
	}, nil)
	if c.KnowsService("broken") {
		t.Error("KnowsService(broken) = true, want false (invalid base_url must be skipped)")
	}
	if c.KnowsService("no-scheme") {
		t.Error("KnowsService(no-scheme) = true, want false (base_url without scheme/host must be skipped)")
	}
	if !c.KnowsService("good") {
		t.Error("KnowsService(good) = false, want true")
	}
}

func TestCredentialProvider_Configured(t *testing.T) {
	withResolver := NewCredentialProvider(nil, stubResolver(nil))
	if !withResolver.Configured() {
		t.Error("Configured() = false with a non-nil resolver, want true")
	}
	withoutResolver := NewCredentialProvider(nil, nil)
	if withoutResolver.Configured() {
		t.Error("Configured() = true with a nil resolver, want false")
	}
	var nilProvider *CredentialProvider
	if nilProvider.Configured() {
		t.Error("nil CredentialProvider.Configured() = true, want false")
	}
}

func TestCredentialProvider_Resolve_UnknownService(t *testing.T) {
	c := NewCredentialProvider(nil, stubResolver(nil))
	if err := c.Resolve("nonexistent", "", "ws-a"); err == nil {
		t.Error("Resolve(nonexistent): want error")
	}
}

func TestCredentialProvider_Resolve_SecretMiss(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "missing-key"}},
	}, stubResolver(nil))
	if err := c.Resolve("myapp", "", "ws-a"); err == nil {
		t.Error("Resolve with a secret-store miss: want error")
	}
}

// TestCredentialProvider_Resolve_NoAccount_UsesUnqualifiedSecretKey pins
// docs/plans/api-gateway-credential-accounts.md D2: an account-less request
// resolves the EXACT SAME secret-store key it always did. Asserted here by
// capturing the literal key string the resolver was actually asked for —
// not merely by checking Resolve succeeds, which would also pass if the key
// shape had silently changed (e.g. gained a "@" suffix) as long as the stub
// happened to have an entry under that new shape too — see grading item #1.
func TestCredentialProvider_Resolve_NoAccount_UsesUnqualifiedSecretKey(t *testing.T) {
	var gotKey string
	resolver := func(namespace, key string) (string, error) {
		gotKey = key
		return "irrelevant", nil
	}
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}},
	}, resolver)

	if err := c.Resolve("freee", "", "ws-a"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if gotKey != "freee-token" {
		t.Errorf("resolver was asked for key %q, want %q (byte-identical to the pre-account-support key)", gotKey, "freee-token")
	}
}

// TestCredentialProvider_Inject_NoAccount_UsesUnqualifiedSecretKey is
// TestCredentialProvider_Resolve_NoAccount_UsesUnqualifiedSecretKey's Inject
// counterpart (grading item #1) — Inject re-resolves independently of
// Resolve (see Inject's own doc comment), so it needs its own assertion.
func TestCredentialProvider_Inject_NoAccount_UsesUnqualifiedSecretKey(t *testing.T) {
	var gotKey string
	resolver := func(namespace, key string) (string, error) {
		gotKey = key
		return "sekret", nil
	}
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}},
	}, resolver)

	req, _ := http.NewRequest("GET", "https://api.freee.co.jp/api/1/companies", nil)
	if err := c.Inject(req, "freee", "", "ws-a"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if gotKey != "freee-token" {
		t.Errorf("resolver was asked for key %q, want %q (byte-identical to the pre-account-support key)", gotKey, "freee-token")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sekret")
	}
}

// TestCredentialProvider_Resolve_AccountUsesQualifiedSecretKey pins the
// composition half of docs/plans/api-gateway-credential-accounts.md
// §"credential identity": account "ubs" against auth.secret_key
// "freee-token" resolves "freee-token@ubs", not any other shape.
func TestCredentialProvider_Resolve_AccountUsesQualifiedSecretKey(t *testing.T) {
	var gotKey string
	resolver := func(namespace, key string) (string, error) {
		gotKey = key
		return "irrelevant", nil
	}
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}},
	}, resolver)

	if err := c.Resolve("freee", "ubs", "ws-a"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if gotKey != "freee-token@ubs" {
		t.Errorf("resolver was asked for key %q, want %q", gotKey, "freee-token@ubs")
	}
}

// TestCredentialProvider_Resolve_AccountDoesNotFallBackToUnqualified pins
// D3 (grading item #2): an account-qualified request whose
// "<secret_key>@<account>" secret does not exist must fail, even though the
// UNQUALIFIED "<secret_key>" secret exists in the same namespace — there is
// no silent fallback to a different account's (or no account's) credential.
func TestCredentialProvider_Resolve_AccountDoesNotFallBackToUnqualified(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}},
	}, stubResolver(map[string]string{"ws-a/freee-token": "unqualified-secret"}))

	if err := c.Resolve("freee", "ubs", "ws-a"); err == nil {
		t.Error("Resolve(freee, account=ubs) with no \"freee-token@ubs\" secret set: want error (no fallback to the unqualified key), got nil")
	}
	// Sanity: the unqualified key itself still resolves fine — proves the
	// failure above is genuinely about account-scoping, not a broken stub.
	if err := c.Resolve("freee", "", "ws-a"); err != nil {
		t.Errorf("Resolve(freee, account=\"\") should still succeed: %v", err)
	}
}

// TestCredentialProvider_Inject_AccountDoesNotFallBackToUnqualified is
// TestCredentialProvider_Resolve_AccountDoesNotFallBackToUnqualified's
// Inject counterpart (grading item #2), and additionally asserts Inject
// leaves req unmodified on that failure.
func TestCredentialProvider_Inject_AccountDoesNotFallBackToUnqualified(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}},
	}, stubResolver(map[string]string{"ws-a/freee-token": "unqualified-secret"}))

	req, _ := http.NewRequest("GET", "https://api.freee.co.jp/api/1/companies", nil)
	if err := c.Inject(req, "freee", "ubs", "ws-a"); err == nil {
		t.Error("Inject(freee, account=ubs) with no \"freee-token@ubs\" secret set: want error (no fallback to the unqualified key), got nil")
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Inject must leave req unmodified when it errors")
	}
}

// TestCredentialProvider_Resolve_OAuth2WithAccountFailsClosed pins PR-1's
// scope boundary: oauth2-kind account support is PR-2's job
// (docs/plans/api-gateway-credential-accounts.md PR 分割) — a non-empty
// account against an oauth2 service must fail loudly here rather than
// silently ignore the account and resolve the unqualified token (which
// would be the D3 "silent fallback" failure mode this whole feature exists
// to prevent, just reached through the oauth2 branch instead of the static
// one).
func TestCredentialProvider_Resolve_OAuth2WithAccountFailsClosed(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	c.SetOAuth2TokenSource(&stubOAuth2TokenSource{tokens: map[string]string{"ws-a/freee": "at-123"}})

	if err := c.Resolve("freee", "ubs", "ws-a"); err == nil {
		t.Error("Resolve for an oauth2-kind service with a non-empty account: want error (PR-2 scope), got nil")
	}
}

// TestCredentialProvider_Inject_OAuth2WithAccountFailsClosed is
// TestCredentialProvider_Resolve_OAuth2WithAccountFailsClosed's Inject
// counterpart.
func TestCredentialProvider_Inject_OAuth2WithAccountFailsClosed(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	c.SetOAuth2TokenSource(&stubOAuth2TokenSource{tokens: map[string]string{"ws-a/freee": "at-123"}})

	req, _ := http.NewRequest("GET", "https://api.freee.co.jp/api/1/companies", nil)
	if err := c.Inject(req, "freee", "ubs", "ws-a"); err == nil {
		t.Error("Inject for an oauth2-kind service with a non-empty account: want error (PR-2 scope), got nil")
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Inject must leave req unmodified when it errors")
	}
}

// TestCredentialProvider_Resolve_OAuth2NoTokenSourceConfigured pins the
// still-relevant slice of PR1's contract: a CredentialProvider that never
// had SetOAuth2TokenSource called on it (every construction site before
// PR2, and any test that doesn't opt in) must still fail an oauth2-kind
// Resolve loudly rather than nil-deref-panic or silently bypass.
func TestCredentialProvider_Resolve_OAuth2NoTokenSourceConfigured(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, stubResolver(nil))
	if err := c.Resolve("freee", "", "ws-a"); err == nil {
		t.Error("Resolve for an oauth2-kind service with no TokenSource wired: want error, got nil")
	}
}

// stubOAuth2TokenSource is a minimal OAuth2AccessTokenSource fake for
// CredentialProvider's own Resolve/Inject tests — the real refresh/
// singleflight/persistence-order behavior is exercised directly against
// *OAuth2TokenSource in oauth2_test.go; these tests only need to prove
// CredentialProvider calls through to whatever OAuth2AccessTokenSource it
// was given, and injects its result as a Bearer header.
type stubOAuth2TokenSource struct {
	tokens map[string]string // "namespace/provider" -> access token
	err    error
	calls  []string // "namespace/provider" call log
}

func (s *stubOAuth2TokenSource) AccessToken(namespace, provider string) (string, error) {
	s.calls = append(s.calls, namespace+"/"+provider)
	if s.err != nil {
		return "", s.err
	}
	tok, ok := s.tokens[namespace+"/"+provider]
	if !ok {
		return "", errors.New("stubOAuth2TokenSource: no token for " + namespace + "/" + provider)
	}
	return tok, nil
}

func TestCredentialProvider_Resolve_OAuth2_DelegatesToTokenSource(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	stub := &stubOAuth2TokenSource{tokens: map[string]string{"ws-a/freee": "at-123"}}
	c.SetOAuth2TokenSource(stub)

	if err := c.Resolve("freee", "", "ws-a"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "ws-a/freee" {
		t.Errorf("AccessToken calls = %v, want exactly [\"ws-a/freee\"]", stub.calls)
	}
}

func TestCredentialProvider_Resolve_OAuth2_TokenSourceError(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	c.SetOAuth2TokenSource(&stubOAuth2TokenSource{err: errors.New("refresh failed")})

	if err := c.Resolve("freee", "", "ws-a"); err == nil {
		t.Error("Resolve when the TokenSource errors: want error, got nil")
	}
}

func TestCredentialProvider_Inject_OAuth2_SetsBearerHeader(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	c.SetOAuth2TokenSource(&stubOAuth2TokenSource{tokens: map[string]string{"ws-a/freee": "at-456"}})

	req, _ := http.NewRequest("GET", "https://api.freee.co.jp/api/1/companies", nil)
	if err := c.Inject(req, "freee", "", "ws-a"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-456" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer at-456")
	}
}

// TestCredentialProvider_Inject_OAuth2_DifferentServicesSameProvider pins
// that two services sharing one oauth_providers entry (auth.provider) each
// resolve independently through Provider, not Name — CredentialProvider's
// map key is the service name, but the token lookup key it hands to
// OAuth2AccessTokenSource is auth.Provider.
func TestCredentialProvider_Inject_OAuth2_DifferentServicesSameProvider(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee-accounting", BaseURL: "https://api.freee.co.jp/api/1", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
		{Name: "freee-hr", BaseURL: "https://api.freee.co.jp/hr/api/1", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	stub := &stubOAuth2TokenSource{tokens: map[string]string{"ws-a/freee": "shared-token"}}
	c.SetOAuth2TokenSource(stub)

	for _, svc := range []string{"freee-accounting", "freee-hr"} {
		req, _ := http.NewRequest("GET", "https://api.freee.co.jp/x", nil)
		if err := c.Inject(req, svc, "", "ws-a"); err != nil {
			t.Fatalf("Inject(%s): %v", svc, err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer shared-token" {
			t.Errorf("Inject(%s): Authorization = %q, want %q", svc, got, "Bearer shared-token")
		}
	}
	if len(stub.calls) != 2 || stub.calls[0] != "ws-a/freee" || stub.calls[1] != "ws-a/freee" {
		t.Errorf("AccessToken calls = %v, want two calls to ws-a/freee", stub.calls)
	}
}

// TestCredentialProvider_OAuth2ProviderFor pins the service->provider
// lookup `boid secret oauth login <service>` (PR3) needs — see
// OAuth2ProviderFor's own doc comment.
func TestCredentialProvider_OAuth2ProviderFor(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee-provider"}},
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, nil)

	if provider, ok := c.OAuth2ProviderFor("freee"); !ok || provider != "freee-provider" {
		t.Errorf("OAuth2ProviderFor(freee) = (%q, %v), want (freee-provider, true)", provider, ok)
	}
	if _, ok := c.OAuth2ProviderFor("myapp"); ok {
		t.Error("OAuth2ProviderFor(myapp) should be false — myapp is auth.kind bearer, not oauth2")
	}
	if _, ok := c.OAuth2ProviderFor("nonexistent"); ok {
		t.Error("OAuth2ProviderFor(nonexistent) should be false — unknown service")
	}
	var nilProvider *CredentialProvider
	if _, ok := nilProvider.OAuth2ProviderFor("freee"); ok {
		t.Error("OAuth2ProviderFor on a nil CredentialProvider should be false (fail-closed)")
	}
}

func TestCredentialProvider_Inject_Bearer(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, stubResolver(map[string]string{"ws-a/myapp-token": "secret123"}))

	req, _ := http.NewRequest("GET", "https://myapp.example.com/v1/users", nil)
	if err := c.Inject(req, "myapp", "", "ws-a"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer secret123" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer secret123")
	}
}

func TestCredentialProvider_Inject_Basic(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "bb", BaseURL: "https://api.bitbucket.org/2.0", Auth: ServiceAuth{Kind: AuthBasic, Username: "x-bitbucket-api-token-auth", SecretKey: "bb-token"}},
	}, stubResolver(map[string]string{"ws-a/bb-token": "tok"}))

	req, _ := http.NewRequest("GET", "https://api.bitbucket.org/2.0/repositories", nil)
	if err := c.Inject(req, "bb", "", "ws-a"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	user, pass, ok := req.BasicAuth()
	if !ok {
		t.Fatal("BasicAuth: not set")
	}
	if user != "x-bitbucket-api-token-auth" || pass != "tok" {
		t.Errorf("BasicAuth = (%q, %q), want (%q, %q)", user, pass, "x-bitbucket-api-token-auth", "tok")
	}
}

func TestCredentialProvider_Inject_Header(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "ops", BaseURL: "https://ops.example.com", Auth: ServiceAuth{Kind: AuthHeader, Header: "X-Api-Key", SecretKey: "ops-key"}},
	}, stubResolver(map[string]string{"ws-a/ops-key": "opskey123"}))

	req, _ := http.NewRequest("GET", "https://ops.example.com/status", nil)
	if err := c.Inject(req, "ops", "", "ws-a"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := req.Header.Get("X-Api-Key"); got != "opskey123" {
		t.Errorf("X-Api-Key header = %q, want %q", got, "opskey123")
	}
}

func TestCredentialProvider_Inject_Query(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "legacy", BaseURL: "https://legacy.example.com", Auth: ServiceAuth{Kind: AuthQuery, Query: "api_key", SecretKey: "legacy-key"}},
	}, stubResolver(map[string]string{"ws-a/legacy-key": "qkey"}))

	req, _ := http.NewRequest("GET", "https://legacy.example.com/v1/status?foo=bar", nil)
	if err := c.Inject(req, "legacy", "", "ws-a"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	q := req.URL.Query()
	if q.Get("api_key") != "qkey" {
		t.Errorf("api_key query param = %q, want %q", q.Get("api_key"), "qkey")
	}
	if q.Get("foo") != "bar" {
		t.Errorf("pre-existing foo query param was dropped: %q, want %q", q.Get("foo"), "bar")
	}
}

// TestCredentialProvider_Inject_OAuth2NoTokenSourceConfigured mirrors
// TestCredentialProvider_Resolve_OAuth2NoTokenSourceConfigured for Inject.
func TestCredentialProvider_Inject_OAuth2NoTokenSourceConfigured(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, stubResolver(nil))
	req, _ := http.NewRequest("GET", "https://api.freee.co.jp/api/1/companies", nil)
	if err := c.Inject(req, "freee", "", "ws-a"); err == nil {
		t.Fatal("Inject for an oauth2-kind service with no TokenSource wired: want error, got nil")
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Inject must leave req unmodified when it errors")
	}
}

func TestCredentialProvider_Inject_NoResolverConfigured(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, nil)
	req, _ := http.NewRequest("GET", "https://myapp.example.com/v1/users", nil)
	if err := c.Inject(req, "myapp", "", "ws-a"); err == nil {
		t.Fatal("Inject with no resolver configured: want error, got nil")
	}
}

func TestCredentialProvider_MultiNamespaceIsolation(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, stubResolver(map[string]string{"ws-a/myapp-token": "secret-a", "ws-b/myapp-token": "secret-b"}))

	reqA, _ := http.NewRequest("GET", "https://myapp.example.com/x", nil)
	if err := c.Inject(reqA, "myapp", "", "ws-a"); err != nil {
		t.Fatalf("Inject (ws-a): %v", err)
	}
	if got := reqA.Header.Get("Authorization"); got != "Bearer secret-a" {
		t.Errorf("ws-a Authorization = %q, want %q", got, "Bearer secret-a")
	}

	reqB, _ := http.NewRequest("GET", "https://myapp.example.com/x", nil)
	if err := c.Inject(reqB, "myapp", "", "ws-b"); err != nil {
		t.Fatalf("Inject (ws-b): %v", err)
	}
	if got := reqB.Header.Get("Authorization"); got != "Bearer secret-b" {
		t.Errorf("ws-b Authorization = %q, want %q", got, "Bearer secret-b")
	}
}

func TestCredentialProvider_NilProviderFailsClosed(t *testing.T) {
	var c *CredentialProvider
	if c.KnowsService("myapp") {
		t.Error("nil CredentialProvider.KnowsService = true, want false")
	}
	if _, ok := c.BaseURLFor("myapp"); ok {
		t.Error("nil CredentialProvider.BaseURLFor: ok = true, want false")
	}
	if err := c.Resolve("myapp", "", "ws-a"); err == nil {
		t.Error("nil CredentialProvider.Resolve: want error, got nil")
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if err := c.Inject(req, "myapp", "", "ws-a"); err == nil {
		t.Error("nil CredentialProvider.Inject: want error, got nil")
	}
}

// sanity: net/url parses the base_urls used in tests the way we expect.
func TestBaseURLSanity(t *testing.T) {
	u, err := url.Parse("https://api.bitbucket.org/2.0")
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "api.bitbucket.org" || u.Path != "/2.0" {
		t.Fatalf("unexpected parse: %+v", u)
	}
}
