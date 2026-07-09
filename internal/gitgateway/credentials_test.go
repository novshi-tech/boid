package gitgateway

import (
	"errors"
	"net/http"
	"testing"
)

func TestCredentialProviderInjectGitHub(t *testing.T) {
	resolver := func(key string) (string, error) {
		if key != "gh-pat" {
			t.Fatalf("resolver called with unexpected key %q", key)
		}
		return "sekrit-token", nil
	}
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, resolver)

	req, _ := http.NewRequest(http.MethodGet, "http://github.com/owner/repo.git/info/refs", nil)
	if err := cp.Inject(req, "github.com"); err != nil {
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
	resolver := func(key string) (string, error) { return "bb-token", nil }
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "bitbucket.org", Forge: ForgeBitbucket, SecretKey: "bb-api-token"},
	}, resolver)

	req, _ := http.NewRequest(http.MethodGet, "http://bitbucket.org/team/repo.git/info/refs", nil)
	if err := cp.Inject(req, "bitbucket.org"); err != nil {
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
	cp := NewCredentialProvider(nil, func(string) (string, error) { return "x", nil })
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/o/r.git/info/refs", nil)
	if err := cp.Inject(req, "example.com"); err == nil {
		t.Fatal("expected error for unconfigured host")
	}
}

func TestCredentialProviderInjectResolverError(t *testing.T) {
	wantErr := errors.New("secret not found")
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "missing"},
	}, func(string) (string, error) { return "", wantErr })

	req, _ := http.NewRequest(http.MethodGet, "http://github.com/o/r.git/info/refs", nil)
	if err := cp.Inject(req, "github.com"); err == nil {
		t.Fatal("expected error to propagate from resolver")
	}
}

func TestCredentialProviderInjectNilResolver(t *testing.T) {
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
	}, nil)
	req, _ := http.NewRequest(http.MethodGet, "http://github.com/o/r.git/info/refs", nil)
	if err := cp.Inject(req, "github.com"); err == nil {
		t.Fatal("expected error when no resolver is configured")
	}
}

func TestCredentialProviderSchemeFor(t *testing.T) {
	cp := NewCredentialProvider([]HostForgeConfig{
		{Host: "github.com", Forge: ForgeGitHub, SecretKey: "gh-pat"},
		{Host: "127.0.0.1:9999", Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, func(string) (string, error) { return "x", nil })

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

func TestNilCredentialProviderFailsClosed(t *testing.T) {
	var cp *CredentialProvider
	req, _ := http.NewRequest(http.MethodGet, "http://github.com/o/r.git/info/refs", nil)
	if err := cp.Inject(req, "github.com"); err == nil {
		t.Fatal("expected error injecting via nil CredentialProvider")
	}
	if got := cp.SchemeFor("github.com"); got != "https" {
		t.Fatalf("nil CredentialProvider SchemeFor = %q, want https default", got)
	}
}
