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
	if err := c.Resolve("nonexistent", "ws-a"); err == nil {
		t.Error("Resolve(nonexistent): want error")
	}
}

func TestCredentialProvider_Resolve_SecretMiss(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "missing-key"}},
	}, stubResolver(nil))
	if err := c.Resolve("myapp", "ws-a"); err == nil {
		t.Error("Resolve with a secret-store miss: want error")
	}
}

func TestCredentialProvider_Resolve_OAuth2NotImplemented(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, stubResolver(nil))
	if err := c.Resolve("freee", "ws-a"); err == nil {
		t.Error("Resolve for an oauth2-kind service (PR1 scope, unimplemented): want error, got nil")
	}
}

func TestCredentialProvider_Inject_Bearer(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, stubResolver(map[string]string{"ws-a/myapp-token": "secret123"}))

	req, _ := http.NewRequest("GET", "https://myapp.example.com/v1/users", nil)
	if err := c.Inject(req, "myapp", "ws-a"); err != nil {
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
	if err := c.Inject(req, "bb", "ws-a"); err != nil {
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
	if err := c.Inject(req, "ops", "ws-a"); err != nil {
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
	if err := c.Inject(req, "legacy", "ws-a"); err != nil {
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

func TestCredentialProvider_Inject_OAuth2NotImplemented(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, stubResolver(nil))
	req, _ := http.NewRequest("GET", "https://api.freee.co.jp/api/1/companies", nil)
	if err := c.Inject(req, "freee", "ws-a"); err == nil {
		t.Fatal("Inject for an oauth2-kind service: want error, got nil")
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
	if err := c.Inject(req, "myapp", "ws-a"); err == nil {
		t.Fatal("Inject with no resolver configured: want error, got nil")
	}
}

func TestCredentialProvider_MultiNamespaceIsolation(t *testing.T) {
	c := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, stubResolver(map[string]string{"ws-a/myapp-token": "secret-a", "ws-b/myapp-token": "secret-b"}))

	reqA, _ := http.NewRequest("GET", "https://myapp.example.com/x", nil)
	if err := c.Inject(reqA, "myapp", "ws-a"); err != nil {
		t.Fatalf("Inject (ws-a): %v", err)
	}
	if got := reqA.Header.Get("Authorization"); got != "Bearer secret-a" {
		t.Errorf("ws-a Authorization = %q, want %q", got, "Bearer secret-a")
	}

	reqB, _ := http.NewRequest("GET", "https://myapp.example.com/x", nil)
	if err := c.Inject(reqB, "myapp", "ws-b"); err != nil {
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
	if err := c.Resolve("myapp", "ws-a"); err == nil {
		t.Error("nil CredentialProvider.Resolve: want error, got nil")
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if err := c.Inject(req, "myapp", "ws-a"); err == nil {
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
