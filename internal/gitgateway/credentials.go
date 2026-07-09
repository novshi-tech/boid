package gitgateway

import (
	"fmt"
	"net/http"
)

// Forge identifies which upstream git host convention to use for Basic auth.
type Forge string

const (
	ForgeGitHub    Forge = "github"
	ForgeBitbucket Forge = "bitbucket"
)

// usernameForForge returns the git-HTTPS Basic-auth username convention for
// a forge (docs/plans/container-based-boid.md 「token 戦略」表):
// GitHub fine-grained PAT uses the conventional "x-access-token"; Bitbucket
// Cloud API tokens use "x-bitbucket-api-token-auth".
func usernameForForge(f Forge) (string, error) {
	switch f {
	case ForgeGitHub:
		return "x-access-token", nil
	case ForgeBitbucket:
		return "x-bitbucket-api-token-auth", nil
	default:
		return "", fmt.Errorf("gitgateway: unrecognized forge %q", f)
	}
}

// SecretResolver resolves a secret-store key reference to its plaintext
// value. config.yaml (PR4) carries only the key reference — never the
// plaintext token (docs/plans/git-gateway-cutover.md: 「config.yaml の
// gateway ブロックは forge 種別と secret key 参照のみ持つ」). This is a
// plain function type, mirroring internal/sandbox.SecretResolver's shape, so
// PR4 can adapt internal/dispatcher.SecretStore.Get to it with a one-line
// closure instead of gitgateway importing the dispatcher package (which
// would drag the sqlite-backed internal/db build into this otherwise
// db-free package).
type SecretResolver func(key string) (string, error)

// HostForgeConfig declares how the gateway authenticates requests to one
// upstream host: which forge convention to use and which secret-store key
// holds the token. "設定 1 フィールドで forge 種別を持てば足りる" per the
// container-based-boid.md token 戦略 section — Forge is that field.
type HostForgeConfig struct {
	// Host is the upstream host as it appears in the gateway route path
	// (e.g. "github.com"), used as the lookup key.
	Host string `yaml:"host"`
	// Forge selects the Basic-auth username convention and (in future
	// callers) any forge-specific behavior.
	Forge Forge `yaml:"forge"`
	// SecretKey is a reference into the secret store
	// (internal/dispatcher/secret_store.go); never a plaintext token.
	SecretKey string `yaml:"secret_key"`
	// Scheme overrides the upstream request scheme; empty means "https".
	// Production forge hosts always use https — this only exists so tests
	// can point a HostForgeConfig at a plaintext httptest upstream.
	Scheme string `yaml:"-"`
}

// CredentialProvider injects forge-appropriate Basic auth into upstream
// requests, resolving the actual token value through a SecretResolver.
type CredentialProvider struct {
	hosts    map[string]HostForgeConfig
	resolver SecretResolver
}

// NewCredentialProvider builds a CredentialProvider from a list of per-host
// forge configs and the resolver used to fetch each config's secret value.
func NewCredentialProvider(hosts []HostForgeConfig, resolver SecretResolver) *CredentialProvider {
	m := make(map[string]HostForgeConfig, len(hosts))
	for _, h := range hosts {
		m[h.Host] = h
	}
	return &CredentialProvider{hosts: m, resolver: resolver}
}

// SchemeFor returns the upstream request scheme for host: the configured
// override if present, otherwise "https".
func (c *CredentialProvider) SchemeFor(host string) string {
	if c == nil {
		return "https"
	}
	if cfg, ok := c.hosts[host]; ok && cfg.Scheme != "" {
		return cfg.Scheme
	}
	return "https"
}

// Inject resolves host's configured secret and sets Basic auth on req using
// the forge's username convention. It returns an error (and leaves req
// unmodified) if host has no configured forge, no resolver is set, or the
// secret can't be resolved — callers log this rather than fail the request
// outright, since a misconfigured host is a config problem, not grounds to
// crash the gateway (docs/plans/git-gateway-cutover.md: 「gateway 自体は
// 落とさない」, said of upstream 401s but applied here in the same spirit).
func (c *CredentialProvider) Inject(req *http.Request, host string) error {
	if c == nil {
		return fmt.Errorf("gitgateway: no credential provider configured")
	}
	cfg, ok := c.hosts[host]
	if !ok {
		return fmt.Errorf("gitgateway: no forge configured for host %q", host)
	}
	if c.resolver == nil {
		return fmt.Errorf("gitgateway: no secret resolver configured for host %q", host)
	}
	token, err := c.resolver(cfg.SecretKey)
	if err != nil {
		return fmt.Errorf("gitgateway: resolve secret %q for host %q: %w", cfg.SecretKey, host, err)
	}
	username, err := usernameForForge(cfg.Forge)
	if err != nil {
		return err
	}
	req.SetBasicAuth(username, token)
	return nil
}
