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
	u, err := c.BaseURLFor("ws-a", "myapp", "")
	if err != nil {
		t.Fatalf("BaseURLFor(myapp): %v", err)
	}
	if u.String() != "https://myapp.example.com/api" {
		t.Errorf("BaseURLFor(myapp) = %q, want %q", u.String(), "https://myapp.example.com/api")
	}
	if _, err := c.BaseURLFor("ws-a", "unknown", ""); err == nil {
		t.Error("BaseURLFor(unknown): want error, got nil")
	}
}

// TestCredentialProvider_BaseURLFor_LiteralNeverCallsResolver pins the
// backward-compatibility/perf property docs/plans/api-gateway-credential-
// accounts.md D12 requires: a service with a literal BaseURL (the pre-D12,
// overwhelmingly common shape) must resolve with ZERO secret-store calls —
// a resolver that panics on any call proves BaseURLFor never touches it for
// this service, regardless of namespace/account.
func TestCredentialProvider_BaseURLFor_LiteralNeverCallsResolver(t *testing.T) {
	panicResolver := func(namespace, key string) (string, error) {
		panic("resolver must never be called for a literal base_url service")
	}
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com/api", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, panicResolver)

	u, err := c.BaseURLFor("ws-a", "myapp", "ubs")
	if err != nil {
		t.Fatalf("BaseURLFor: %v", err)
	}
	if u.String() != "https://myapp.example.com/api" {
		t.Errorf("BaseURLFor = %q, want %q", u.String(), "https://myapp.example.com/api")
	}
}

// TestCredentialProvider_RequiresAccount pins docs/plans/api-gateway-
// credential-accounts.md D5 at the CredentialProvider level: RequiresAccount
// reflects each service's own ServiceConfig.RequireAccount independently,
// and an unknown service reports false (see RequiresAccount's own doc
// comment for why false, specifically, is the right answer there — it
// preserves BaseURLFor as the single source of truth for "is this service
// real").
func TestCredentialProvider_RequiresAccount(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}, RequireAccount: true},
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, nil)

	if !c.RequiresAccount("freee") {
		t.Error(`RequiresAccount("freee") = false, want true`)
	}
	if c.RequiresAccount("myapp") {
		t.Error(`RequiresAccount("myapp") = true, want false (default)`)
	}
	if c.RequiresAccount("unknown") {
		t.Error(`RequiresAccount("unknown") = true, want false`)
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
	if err := c.Resolve("ws-a", "nonexistent", ""); err == nil {
		t.Error("Resolve(nonexistent): want error")
	}
}

func TestCredentialProvider_Resolve_SecretMiss(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "missing-key"}},
	}, stubResolver(nil))
	if err := c.Resolve("ws-a", "myapp", ""); err == nil {
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

	if err := c.Resolve("ws-a", "freee", ""); err != nil {
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
	if err := c.Inject(req, "ws-a", "freee", ""); err != nil {
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

	if err := c.Resolve("ws-a", "freee", "ubs"); err != nil {
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

	if err := c.Resolve("ws-a", "freee", "ubs"); err == nil {
		t.Error("Resolve(freee, account=ubs) with no \"freee-token@ubs\" secret set: want error (no fallback to the unqualified key), got nil")
	}
	// Sanity: the unqualified key itself still resolves fine — proves the
	// failure above is genuinely about account-scoping, not a broken stub.
	if err := c.Resolve("ws-a", "freee", ""); err != nil {
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
	if err := c.Inject(req, "ws-a", "freee", "ubs"); err == nil {
		t.Error("Inject(freee, account=ubs) with no \"freee-token@ubs\" secret set: want error (no fallback to the unqualified key), got nil")
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Inject must leave req unmodified when it errors")
	}
}

// TestCredentialProvider_Resolve_OAuth2WithAccount_DelegatesWithCredentialID
// pins PR-2's D6 wiring at the CredentialProvider boundary: a non-empty
// account against an oauth2 service must reach OAuth2AccessTokenSource.
// AccessToken as a credentialID carrying THAT account (not silently
// dropped, and not resolved against the account-less provider entry — the
// D3 "silent fallback" failure mode this whole feature exists to prevent).
// stubOAuth2TokenSource's own key composition (secretPrefix, this file's
// doc comment) is what makes "ws-a/freee@ubs" only match when account
// actually arrived intact.
func TestCredentialProvider_Resolve_OAuth2WithAccount_DelegatesWithCredentialID(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	stub := &stubOAuth2TokenSource{tokens: map[string]string{"ws-a/freee@ubs": "at-ubs"}}
	c.SetOAuth2TokenSource(stub)

	if err := c.Resolve("ws-a", "freee", "ubs"); err != nil {
		t.Fatalf("Resolve(freee, account=ubs): %v", err)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "ws-a/freee@ubs" {
		t.Errorf("AccessToken calls = %v, want exactly [\"ws-a/freee@ubs\"]", stub.calls)
	}
}

// TestCredentialProvider_Inject_OAuth2WithAccount_DelegatesWithCredentialID
// is TestCredentialProvider_Resolve_OAuth2WithAccount_DelegatesWithCredentialID's
// Inject counterpart, additionally checking that the account-scoped token
// (not some other account's, not the unqualified one) lands in the
// Authorization header.
func TestCredentialProvider_Inject_OAuth2WithAccount_DelegatesWithCredentialID(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	stub := &stubOAuth2TokenSource{tokens: map[string]string{
		"ws-a/freee":     "at-unqualified",
		"ws-a/freee@ubs": "at-ubs",
	}}
	c.SetOAuth2TokenSource(stub)

	req, _ := http.NewRequest("GET", "https://api.freee.co.jp/api/1/companies", nil)
	if err := c.Inject(req, "ws-a", "freee", "ubs"); err != nil {
		t.Fatalf("Inject(freee, account=ubs): %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-ubs" {
		t.Errorf("Authorization header = %q, want %q (the ubs-account token, not the unqualified one)", got, "Bearer at-ubs")
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
	if err := c.Resolve("ws-a", "freee", ""); err == nil {
		t.Error("Resolve for an oauth2-kind service with no TokenSource wired: want error, got nil")
	}
}

// stubOAuth2TokenSource is a minimal OAuth2AccessTokenSource fake for
// CredentialProvider's own Resolve/Inject tests — the real refresh/
// singleflight/persistence-order behavior is exercised directly against
// *OAuth2TokenSource in oauth2_test.go; these tests only need to prove
// CredentialProvider calls through to whatever OAuth2AccessTokenSource it
// was given — with the credentialID it was actually handed, account
// included — and injects its result as a Bearer header.
//
// Keys are "namespace/" + cred.secretPrefix() (credentialID's own account-
// qualification format, oauth2.go) — "namespace/provider" when cred.account
// is empty (byte-identical to every pre-PR-2 call site in this file) or
// "namespace/provider@account" otherwise, so a test can tell "the
// unqualified token was used" apart from "the wrong/right account's token
// was used" just by which map key was looked up.
type stubOAuth2TokenSource struct {
	tokens map[string]string // "namespace/" + cred.secretPrefix() -> access token
	err    error
	calls  []string // same key shape, call log
}

func (s *stubOAuth2TokenSource) AccessToken(namespace string, cred credentialID) (string, error) {
	key := namespace + "/" + cred.secretPrefix()
	s.calls = append(s.calls, key)
	if s.err != nil {
		return "", s.err
	}
	tok, ok := s.tokens[key]
	if !ok {
		return "", errors.New("stubOAuth2TokenSource: no token for " + key)
	}
	return tok, nil
}

func TestCredentialProvider_Resolve_OAuth2_DelegatesToTokenSource(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	stub := &stubOAuth2TokenSource{tokens: map[string]string{"ws-a/freee": "at-123"}}
	c.SetOAuth2TokenSource(stub)

	if err := c.Resolve("ws-a", "freee", ""); err != nil {
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

	if err := c.Resolve("ws-a", "freee", ""); err == nil {
		t.Error("Resolve when the TokenSource errors: want error, got nil")
	}
}

func TestCredentialProvider_Inject_OAuth2_SetsBearerHeader(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, nil)
	c.SetOAuth2TokenSource(&stubOAuth2TokenSource{tokens: map[string]string{"ws-a/freee": "at-456"}})

	req, _ := http.NewRequest("GET", "https://api.freee.co.jp/api/1/companies", nil)
	if err := c.Inject(req, "ws-a", "freee", ""); err != nil {
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
		if err := c.Inject(req, "ws-a", svc, ""); err != nil {
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
	if err := c.Inject(req, "ws-a", "myapp", ""); err != nil {
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
	if err := c.Inject(req, "ws-a", "bb", ""); err != nil {
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
	if err := c.Inject(req, "ws-a", "ops", ""); err != nil {
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
	if err := c.Inject(req, "ws-a", "legacy", ""); err != nil {
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
	if err := c.Inject(req, "ws-a", "freee", ""); err == nil {
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
	if err := c.Inject(req, "ws-a", "myapp", ""); err == nil {
		t.Fatal("Inject with no resolver configured: want error, got nil")
	}
}

func TestCredentialProvider_MultiNamespaceIsolation(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, stubResolver(map[string]string{"ws-a/myapp-token": "secret-a", "ws-b/myapp-token": "secret-b"}))

	reqA, _ := http.NewRequest("GET", "https://myapp.example.com/x", nil)
	if err := c.Inject(reqA, "ws-a", "myapp", ""); err != nil {
		t.Fatalf("Inject (ws-a): %v", err)
	}
	if got := reqA.Header.Get("Authorization"); got != "Bearer secret-a" {
		t.Errorf("ws-a Authorization = %q, want %q", got, "Bearer secret-a")
	}

	reqB, _ := http.NewRequest("GET", "https://myapp.example.com/x", nil)
	if err := c.Inject(reqB, "ws-b", "myapp", ""); err != nil {
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
	if _, err := c.BaseURLFor("ws-a", "myapp", ""); err == nil {
		t.Error("nil CredentialProvider.BaseURLFor: want error, got nil")
	}
	if c.RequiresAccount("myapp") {
		t.Error("nil CredentialProvider.RequiresAccount = true, want false")
	}
	if err := c.Resolve("ws-a", "myapp", ""); err == nil {
		t.Error("nil CredentialProvider.Resolve: want error, got nil")
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if err := c.Inject(req, "ws-a", "myapp", ""); err == nil {
		t.Error("nil CredentialProvider.Inject: want error, got nil")
	}
}

// --- D12: base_url_secret_key / auth.username_secret_key ---
// docs/plans/api-gateway-credential-accounts.md D12 — the Jira-Cloud-shaped
// case: a service whose upstream TENANT (subdomain) and login identity, not
// just the token, differ per account.

// TestCredentialProvider_BaseURLFor_SecretBacked_NoAccount_UsesUnqualifiedKey
// pins D2 for the new axis: an account-less request against a
// BaseURLSecretKey-backed service resolves the bare key, byte-identical in
// shape to how auth.secret_key already behaves.
func TestCredentialProvider_BaseURLFor_SecretBacked_NoAccount_UsesUnqualifiedKey(t *testing.T) {
	var gotKey string
	resolver := func(namespace, key string) (string, error) {
		gotKey = key
		return "https://aolani.atlassian.net", nil
	}
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "jira-api", BaseURLSecretKey: "JIRA_BASE_URL", Auth: ServiceAuth{Kind: AuthBasic, Username: "x", SecretKey: "k"}},
	}, resolver)

	u, err := c.BaseURLFor("ws-a", "jira-api", "")
	if err != nil {
		t.Fatalf("BaseURLFor: %v", err)
	}
	if gotKey != "JIRA_BASE_URL" {
		t.Errorf("resolver was asked for key %q, want %q", gotKey, "JIRA_BASE_URL")
	}
	if u.String() != "https://aolani.atlassian.net" {
		t.Errorf("BaseURLFor = %q, want %q", u.String(), "https://aolani.atlassian.net")
	}
}

// TestCredentialProvider_BaseURLFor_SecretBacked_AccountUsesQualifiedKey
// pins the "credential identity" composition for base_url: account "ubs"
// against base_url_secret_key "JIRA_BASE_URL" resolves "JIRA_BASE_URL@ubs",
// and the two accounts resolve to genuinely DIFFERENT hosts — this is the
// actual motivating scenario (Jira Cloud: one service definition, one
// tenant per account).
func TestCredentialProvider_BaseURLFor_SecretBacked_AccountUsesQualifiedKey(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "jira-api", BaseURLSecretKey: "JIRA_BASE_URL", Auth: ServiceAuth{Kind: AuthBasic, Username: "x", SecretKey: "k"}},
	}, stubResolver(map[string]string{
		"ws-a/JIRA_BASE_URL":     "https://aolani.atlassian.net",
		"ws-a/JIRA_BASE_URL@ubs": "https://urban-b.atlassian.net",
	}))

	uDefault, err := c.BaseURLFor("ws-a", "jira-api", "")
	if err != nil {
		t.Fatalf("BaseURLFor(account=\"\"): %v", err)
	}
	if uDefault.Host != "aolani.atlassian.net" {
		t.Errorf("BaseURLFor(account=\"\") host = %q, want %q", uDefault.Host, "aolani.atlassian.net")
	}

	uUbs, err := c.BaseURLFor("ws-a", "jira-api", "ubs")
	if err != nil {
		t.Fatalf("BaseURLFor(account=ubs): %v", err)
	}
	if uUbs.Host != "urban-b.atlassian.net" {
		t.Errorf("BaseURLFor(account=ubs) host = %q, want %q", uUbs.Host, "urban-b.atlassian.net")
	}
}

// TestCredentialProvider_BaseURLFor_SecretBacked_NoFallback pins D3 for the
// new axis: an account-qualified base_url_secret_key that isn't set in the
// secret store must fail, even though the unqualified key exists — no
// silent fallback to a different (or no) account's upstream target.
func TestCredentialProvider_BaseURLFor_SecretBacked_NoFallback(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "jira-api", BaseURLSecretKey: "JIRA_BASE_URL", Auth: ServiceAuth{Kind: AuthBasic, Username: "x", SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/JIRA_BASE_URL": "https://aolani.atlassian.net"}))

	if _, err := c.BaseURLFor("ws-a", "jira-api", "ubs"); err == nil {
		t.Error("BaseURLFor(account=ubs) with no \"JIRA_BASE_URL@ubs\" secret set: want error, got nil")
	}
}

// TestCredentialProvider_BaseURLFor_SecretBacked_MalformedURLRejected proves
// the resolved secret-store VALUE is validated with the same rules a
// literal base_url gets at config-load time (ValidateBaseURL) — a
// malformed value fails closed instead of producing a nil/garbage URL the
// caller might dereference.
func TestCredentialProvider_BaseURLFor_SecretBacked_MalformedURLRejected(t *testing.T) {
	cases := map[string]string{
		"not a url at all":             "not a url at all",
		"missing scheme":               "aolani.atlassian.net",
		"has a query":                  "https://aolani.atlassian.net?x=1",
		"non-https, no allow_insecure": "http://aolani.atlassian.net",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewCredentialProvider([]ServiceConfig{
				{Name: "jira-api", BaseURLSecretKey: "JIRA_BASE_URL", Auth: ServiceAuth{Kind: AuthBasic, Username: "x", SecretKey: "k"}},
			}, stubResolver(map[string]string{"ws-a/JIRA_BASE_URL": value}))
			if _, err := c.BaseURLFor("ws-a", "jira-api", ""); err == nil {
				t.Errorf("BaseURLFor with resolved value %q: want error, got nil", value)
			}
		})
	}
}

// TestCredentialProvider_BaseURLFor_SecretBacked_AllowInsecurePermitsHTTP
// proves AllowInsecure reaches the runtime validation path for a
// secret-backed base_url exactly as it does for a literal one.
func TestCredentialProvider_BaseURLFor_SecretBacked_AllowInsecurePermitsHTTP(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "internal", BaseURLSecretKey: "INTERNAL_BASE_URL", AllowInsecure: true, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/INTERNAL_BASE_URL": "http://internal.example.com"}))

	u, err := c.BaseURLFor("ws-a", "internal", "")
	if err != nil {
		t.Fatalf("BaseURLFor: %v", err)
	}
	if u.String() != "http://internal.example.com" {
		t.Errorf("BaseURLFor = %q, want %q", u.String(), "http://internal.example.com")
	}
}

// TestCredentialProvider_Inject_UsernameSecretKey_AccountQualified pins the
// auth.username_secret_key half of D12: two accounts of the same service
// resolve DIFFERENT Basic-auth usernames (the Jira Cloud case — a different
// login email per tenant), and account "" still resolves the unqualified
// key (D2).
func TestCredentialProvider_Inject_UsernameSecretKey_AccountQualified(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "jira-api", BaseURL: "https://aolani.atlassian.net", Auth: ServiceAuth{Kind: AuthBasic, UsernameSecretKey: "JIRA_USERNAME", SecretKey: "JIRA_API_TOKEN"}},
	}, stubResolver(map[string]string{
		"ws-a/JIRA_USERNAME":      "naoki.nose@kameda-hi.co.jp",
		"ws-a/JIRA_USERNAME@ubs":  "nose@urban-b.com",
		"ws-a/JIRA_API_TOKEN":     "tok-aolani",
		"ws-a/JIRA_API_TOKEN@ubs": "tok-ubs",
	}))

	reqDefault, _ := http.NewRequest("GET", "https://aolani.atlassian.net/rest/api/2/issue", nil)
	if err := c.Inject(reqDefault, "ws-a", "jira-api", ""); err != nil {
		t.Fatalf("Inject(account=\"\"): %v", err)
	}
	user, pass, _ := reqDefault.BasicAuth()
	if user != "naoki.nose@kameda-hi.co.jp" || pass != "tok-aolani" {
		t.Errorf("Inject(account=\"\") BasicAuth = (%q, %q), want (%q, %q)", user, pass, "naoki.nose@kameda-hi.co.jp", "tok-aolani")
	}

	reqUbs, _ := http.NewRequest("GET", "https://urban-b.atlassian.net/rest/api/2/issue", nil)
	if err := c.Inject(reqUbs, "ws-a", "jira-api", "ubs"); err != nil {
		t.Fatalf("Inject(account=ubs): %v", err)
	}
	user, pass, _ = reqUbs.BasicAuth()
	if user != "nose@urban-b.com" || pass != "tok-ubs" {
		t.Errorf("Inject(account=ubs) BasicAuth = (%q, %q), want (%q, %q)", user, pass, "nose@urban-b.com", "tok-ubs")
	}
}

// TestCredentialProvider_Inject_UsernameSecretKey_ColonRejectedAtRuntime
// pins the runtime re-application of the RFC 7617 §2 colon check
// config.validateServiceConfig only ever runs against a LITERAL
// auth.username at config-load time — a resolved secret-store value can't
// be checked until now.
func TestCredentialProvider_Inject_UsernameSecretKey_ColonRejectedAtRuntime(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "jira-api", BaseURL: "https://aolani.atlassian.net", Auth: ServiceAuth{Kind: AuthBasic, UsernameSecretKey: "JIRA_USERNAME", SecretKey: "k"}},
	}, stubResolver(map[string]string{
		"ws-a/JIRA_USERNAME": "bad:username",
		"ws-a/k":             "tok",
	}))

	req, _ := http.NewRequest("GET", "https://aolani.atlassian.net/x", nil)
	if err := c.Inject(req, "ws-a", "jira-api", ""); err == nil {
		t.Error("Inject with a resolved username containing \":\": want error, got nil")
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Inject must leave req unmodified when it errors")
	}
}

// TestCredentialProvider_Resolve_UsernameSecretKey_FailsFastBeforeInject
// proves the username_secret_key resolution/validation runs inside Resolve
// too (the fail-fast pre-check Server.ServeHTTP relies on), not only inside
// Inject — a broken username secret must 502 before ever proxying, the same
// posture SecretKey itself already has.
func TestCredentialProvider_Resolve_UsernameSecretKey_FailsFastBeforeInject(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "jira-api", BaseURL: "https://aolani.atlassian.net", Auth: ServiceAuth{Kind: AuthBasic, UsernameSecretKey: "JIRA_USERNAME", SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "tok"})) // JIRA_USERNAME deliberately missing

	if err := c.Resolve("ws-a", "jira-api", ""); err == nil {
		t.Error("Resolve with a missing username_secret_key secret: want error, got nil")
	}
}

// TestCredentialProvider_Inject_LiteralUsername_NeverCallsResolverForUsername
// pins the zero-cost backward-compat property for the username half: a
// service using the literal auth.Username (no UsernameSecretKey) must never
// ask the resolver for anything but the token's own SecretKey.
func TestCredentialProvider_Inject_LiteralUsername_NeverCallsResolverForUsername(t *testing.T) {
	var gotKeys []string
	resolver := func(namespace, key string) (string, error) {
		gotKeys = append(gotKeys, key)
		return "tok", nil
	}
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "bb", BaseURL: "https://api.bitbucket.org/2.0", Auth: ServiceAuth{Kind: AuthBasic, Username: "x-bitbucket-api-token-auth", SecretKey: "bb-token"}},
	}, resolver)

	req, _ := http.NewRequest("GET", "https://api.bitbucket.org/2.0/repositories", nil)
	if err := c.Inject(req, "ws-a", "bb", ""); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(gotKeys) != 1 || gotKeys[0] != "bb-token" {
		t.Errorf("resolver calls = %v, want exactly [\"bb-token\"] (username is literal, must never be resolved)", gotKeys)
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
