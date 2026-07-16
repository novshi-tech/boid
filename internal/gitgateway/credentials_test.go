package gitgateway

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCredentialProviderInjectGitHub(t *testing.T) {
	resolver := func(namespace, key string) (string, error) {
		if key != "gh-pat" {
			t.Fatalf("resolver called with unexpected key %q", key)
		}
		if namespace != "ws-1" {
			t.Fatalf("resolver called with unexpected namespace %q, want ws-1", namespace)
		}
		return "sekrit-token", nil
	}
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, resolver)

	req, _ := http.NewRequest(http.MethodGet, "http://github.com/owner/repo.git/info/refs", nil)
	if err := cp.Inject(req, "github.com", "ws-1"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	user, pass, ok := req.BasicAuth()
	if !ok {
		t.Fatal("expected Basic auth to be set")
	}
	if user != "x-access-token" {
		t.Fatalf("username = %q, want x-access-token", user)
	}
	if pass != "sekrit-token" {
		t.Fatalf("password = %q, want sekrit-token", pass)
	}
}

func TestCredentialProviderInjectBitbucket(t *testing.T) {
	resolver := func(namespace, key string) (string, error) { return "bb-token", nil }
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "bitbucket.org", Forge: ForgeBitbucket, SecretKey: "bb-api-token"},
	}, resolver)

	req, _ := http.NewRequest(http.MethodGet, "http://bitbucket.org/team/repo.git/info/refs", nil)
	if err := cp.Inject(req, "bitbucket.org", "default"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	user, pass, ok := req.BasicAuth()
	if !ok {
		t.Fatal("expected Basic auth to be set")
	}
	if user != "x-bitbucket-api-token-auth" {
		t.Fatalf("username = %q, want x-bitbucket-api-token-auth", user)
	}
	if pass != "bb-token" {
		t.Fatalf("password = %q, want bb-token", pass)
	}
}

func TestCredentialProviderInjectUnknownHost(t *testing.T) {
	cp := NewCredentialProvider(nil, func(string, string) (string, error) { return "x", nil })
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/o/r.git/info/refs", nil)
	if err := cp.Inject(req, "example.com", "default"); err == nil {
		t.Fatal("expected error for unconfigured host")
	}
}

func TestCredentialProviderInjectResolverError(t *testing.T) {
	wantErr := errors.New("secret not found")
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "missing"},
	}, func(string, string) (string, error) { return "", wantErr })

	req, _ := http.NewRequest(http.MethodGet, "http://github.com/o/r.git/info/refs", nil)
	if err := cp.Inject(req, "github.com", "default"); err == nil {
		t.Fatal("expected error to propagate from resolver")
	}
}

func TestCredentialProviderInjectNilResolver(t *testing.T) {
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, nil)
	req, _ := http.NewRequest(http.MethodGet, "http://github.com/o/r.git/info/refs", nil)
	if err := cp.Inject(req, "github.com", "default"); err == nil {
		t.Fatal("expected error when no resolver is configured")
	}
}

// TestCredentialProviderInjectNamespaceRoutesToDifferentSecret proves the
// namespace parameter actually reaches the resolver and selects a distinct
// secret per namespace — the crux of post-cutover 改善 §1 (workspace-scoped
// PAT namespace): two workspaces sharing one gateway host config must be
// able to authenticate with two different PATs.
func TestCredentialProviderInjectNamespaceRoutesToDifferentSecret(t *testing.T) {
	secrets := map[string]string{
		"ws-a": "pat-for-ws-a",
		"ws-b": "pat-for-ws-b",
	}
	resolver := func(namespace, key string) (string, error) {
		if key != "gh-pat" {
			t.Fatalf("resolver called with unexpected key %q", key)
		}
		v, ok := secrets[namespace]
		if !ok {
			t.Fatalf("resolver called with unexpected namespace %q", namespace)
		}
		return v, nil
	}
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, resolver)

	for namespace, wantToken := range secrets {
		req, _ := http.NewRequest(http.MethodGet, "http://github.com/owner/repo.git/info/refs", nil)
		if err := cp.Inject(req, "github.com", namespace); err != nil {
			t.Fatalf("Inject(namespace=%q): %v", namespace, err)
		}
		_, pass, ok := req.BasicAuth()
		if !ok {
			t.Fatalf("namespace %q: expected Basic auth to be set", namespace)
		}
		if pass != wantToken {
			t.Fatalf("namespace %q: password = %q, want %q", namespace, pass, wantToken)
		}
	}
}

func TestCredentialProviderSchemeFor(t *testing.T) {
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
		{Host: "127.0.0.1:9999", Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, func(string, string) (string, error) { return "x", nil })

	if got := cp.SchemeFor("github.com"); got != "https" {
		t.Fatalf("SchemeFor(github.com) = %q, want https", got)
	}
	if got := cp.SchemeFor("127.0.0.1:9999"); got != "http" {
		t.Fatalf("SchemeFor(127.0.0.1:9999) = %q, want http", got)
	}
	if got := cp.SchemeFor("unconfigured.example"); got != "https" {
		t.Fatalf("SchemeFor(unconfigured) = %q, want https default", got)
	}
}

func TestCredentialProviderConfigured(t *testing.T) {
	withResolver := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, func(string, string) (string, error) { return "x", nil })
	if !withResolver.Configured() {
		t.Error("Configured() = false, want true when a resolver is wired")
	}

	// internal/server/wire.go builds exactly this shape when
	// config.KeyFilePath (and therefore the secret store) is unset —
	// hosts may still be configured, but there is no resolver at all.
	noResolver := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, nil)
	if noResolver.Configured() {
		t.Error("Configured() = true, want false when resolver is nil")
	}

	var nilProvider *CredentialProvider
	if nilProvider.Configured() {
		t.Error("Configured() = true, want false for a nil *CredentialProvider")
	}
}

func TestNilCredentialProviderFailsClosed(t *testing.T) {
	var cp *CredentialProvider
	req, _ := http.NewRequest(http.MethodGet, "http://github.com/o/r.git/info/refs", nil)
	if err := cp.Inject(req, "github.com", "default"); err == nil {
		t.Fatal("expected error injecting via nil CredentialProvider")
	}
	if got := cp.SchemeFor("github.com"); got != "https" {
		t.Fatalf("nil CredentialProvider SchemeFor = %q, want https default", got)
	}
}

// The Resolve tests below cover the "pre-check" surface introduced in PR-A
// (docs/plans/gitgateway-credential-fail-fast.md). Inject is now a thin
// wrapper over Resolve, so its own test cases (above) transitively exercise
// the same code paths — these tests add coverage for callers that will hit
// Resolve directly (Server.ServeHTTP's fail-fast pre-check, PR-B).

func TestResolveGitHub(t *testing.T) {
	resolver := func(namespace, key string) (string, error) {
		if key != "gh-pat" {
			t.Fatalf("resolver called with unexpected key %q", key)
		}
		if namespace != "ws-1" {
			t.Fatalf("resolver called with unexpected namespace %q, want ws-1", namespace)
		}
		return "sekrit-token", nil
	}
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, resolver)

	user, token, err := cp.Resolve("github.com", "ws-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if user != "x-access-token" {
		t.Fatalf("username = %q, want x-access-token", user)
	}
	if token != "sekrit-token" {
		t.Fatalf("token = %q, want sekrit-token", token)
	}
}

func TestResolveBitbucket(t *testing.T) {
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "bitbucket.org", Forge: ForgeBitbucket, SecretKey: "bb-api-token"},
	}, func(string, string) (string, error) { return "bb-token", nil })

	user, token, err := cp.Resolve("bitbucket.org", "default")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if user != "x-bitbucket-api-token-auth" {
		t.Fatalf("username = %q, want x-bitbucket-api-token-auth", user)
	}
	if token != "bb-token" {
		t.Fatalf("token = %q, want bb-token", token)
	}
}

func TestResolveUnknownHost(t *testing.T) {
	cp := NewCredentialProvider(nil, func(string, string) (string, error) { return "x", nil })
	user, token, err := cp.Resolve("example.com", "default")
	if err == nil {
		t.Fatal("expected error for unconfigured host")
	}
	if user != "" || token != "" {
		t.Fatalf("Resolve(err path) = (%q, %q), want empty strings", user, token)
	}
}

func TestResolveResolverError(t *testing.T) {
	wantErr := errors.New("no rows in result set")
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "bitbucket.org", Forge: ForgeBitbucket, SecretKey: "BB_TOKEN"},
	}, func(string, string) (string, error) { return "", wantErr })

	user, token, err := cp.Resolve("bitbucket.org", "khi")
	if err == nil {
		t.Fatal("expected error to propagate from resolver")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Resolve err = %v, want to wrap %v", err, wantErr)
	}
	// The 502 body composed by Server.ServeHTTP (PR-B) needs enough hints
	// for nose to locate the missing secret (host + namespace + secret key
	// name), so keep those substrings in the wrapped error message.
	msg := err.Error()
	for _, want := range []string{"bitbucket.org", "khi", "BB_TOKEN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Resolve err message = %q, missing %q", msg, want)
		}
	}
	if user != "" || token != "" {
		t.Fatalf("Resolve(err path) = (%q, %q), want empty strings", user, token)
	}
}

func TestResolveNilResolver(t *testing.T) {
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, nil)
	_, _, err := cp.Resolve("github.com", "default")
	if err == nil {
		t.Fatal("expected error when no resolver is configured")
	}
}

func TestResolveNilProvider(t *testing.T) {
	var cp *CredentialProvider
	_, _, err := cp.Resolve("github.com", "default")
	if err == nil {
		t.Fatal("expected error when calling Resolve on nil provider")
	}
}

// KnowsHost is what Server.ServeHTTP uses to gate the fail-fast pre-check
// (docs/plans/gitgateway-credential-fail-fast.md PR-B): only known hosts
// take the 502 path, unknown hosts fall through to Rewrite's pre-existing
// fail-open + notify behavior — exactly what e2e's httptest.Server dynamic
// ports depend on. Wrong answers here reappear as either regressed 502s in
// e2e or a lost hang-guard in production; both cost real debugging time.
func TestKnowsHost(t *testing.T) {
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
		{Host: "bitbucket.org", Forge: ForgeBitbucket, SecretKey: "BB_TOKEN"},
	}, func(string, string) (string, error) { return "x", nil })

	if !cp.KnowsHost("github.com") {
		t.Error("KnowsHost(github.com) = false, want true")
	}
	if !cp.KnowsHost("bitbucket.org") {
		t.Error("KnowsHost(bitbucket.org) = false, want true")
	}
	if cp.KnowsHost("127.0.0.1:40033") {
		t.Error("KnowsHost(unconfigured) = true, want false (test upstreams / unregistered forges must be considered unknown so the fail-fast pre-check skips them)")
	}
	var nilProvider *CredentialProvider
	if nilProvider.KnowsHost("github.com") {
		t.Error("KnowsHost on nil provider = true, want false")
	}
}
