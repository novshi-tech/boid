package gitgateway

import (
	"errors"
	"net/http"
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
