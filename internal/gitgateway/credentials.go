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
// a forge: GitHub fine-grained PAT uses the conventional "x-access-token";
// Bitbucket Cloud API tokens use "x-bitbucket-api-token-auth".
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

// SecretResolver resolves a secret-store key reference, scoped to a
// namespace, to its plaintext value. config.yaml carries only the key
// reference, never the plaintext token. This is a plain function type,
// mirroring internal/sandbox.SecretResolver's shape, so callers can adapt
// internal/dispatcher.SecretStore.Get to it with a one-line closure instead
// of gitgateway importing the dispatcher package. namespace is the job
// token's Registry-recorded namespace (Entry.Namespace); an empty namespace
// is expected to fall back to a "default" namespace (mirroring
// internal/dispatcher.SecretStore.Get's own normalizeNamespace).
type SecretResolver func(namespace, key string) (string, error)

// HostForgeConfig declares how the gateway authenticates requests to one
// upstream host: which forge convention to use and which secret-store key
// holds the token.
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

// Configured reports whether c has any secret resolver wired at all. It
// distinguishes two failure modes at the caller (Server.ServeHTTP):
//
//   - resolver == nil: no secret store configured at all — a systemic
//     "credentials aren't set up yet" state, true for every host/request.
//   - resolver present but errors for a specific key: an ordinary per-key
//     miss (e.g. a HostForgeConfig.SecretKey reference that was never
//     `boid secret set`), which Inject still surfaces per-request via its
//     usual error return.
//
// This lets ServeHTTP fail fast for the former without spamming
// NotifyCredentialError on every request, leaving the latter's existing
// fail-open + notify behavior unchanged.
func (c *CredentialProvider) Configured() bool {
	return c != nil && c.resolver != nil
}

// KnowsHost reports whether c has a HostForgeConfig entry for host — i.e.
// whether host is in the gateway's allowlist of forges the daemon is
// configured to authenticate. Server.ServeHTTP's fail-fast pre-check uses
// this to distinguish "host we have no opinion on" (e.g. test upstreams;
// forwarded unauthenticated, as before) from "host we're configured for but
// whose secret fails to resolve" (502, no forwarding).
//
// A nil *CredentialProvider knows no hosts, matching the fail-closed
// behavior of Inject/Resolve above.
func (c *CredentialProvider) KnowsHost(host string) bool {
	if c == nil {
		return false
	}
	_, ok := c.hosts[host]
	return ok
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

// Resolve looks up host's forge config, resolves its secret key against
// namespace, and returns the Basic-auth username / token pair the gateway
// would use for outbound requests to that host. It returns an error (leaving
// both strings empty) if host has no configured forge, no resolver is set,
// or the secret can't be resolved.
//
// Server.ServeHTTP calls Resolve before proxying so it can 502 without ever
// contacting the upstream when credentials fail to resolve, avoiding the
// sandbox-side git credential prompt that upstream 401s cause. Inject below
// is a thin wrapper for the Rewrite callback path that still needs to
// SetBasicAuth on an outbound *http.Request.
func (c *CredentialProvider) Resolve(host, namespace string) (username, token string, err error) {
	if c == nil {
		return "", "", fmt.Errorf("gitgateway: no credential provider configured")
	}
	cfg, ok := c.hosts[host]
	if !ok {
		return "", "", fmt.Errorf("gitgateway: no forge configured for host %q", host)
	}
	if c.resolver == nil {
		return "", "", fmt.Errorf("gitgateway: no secret resolver configured for host %q", host)
	}
	tok, err := c.resolver(namespace, cfg.SecretKey)
	if err != nil {
		return "", "", fmt.Errorf("gitgateway: resolve secret %q for host %q (namespace %q): %w", cfg.SecretKey, host, namespace, err)
	}
	user, err := usernameForForge(cfg.Forge)
	if err != nil {
		return "", "", err
	}
	return user, tok, nil
}

// Inject resolves host's configured secret, scoped to namespace, and sets
// Basic auth on req using the forge's username convention. It returns an
// error (and leaves req unmodified) if host has no configured forge, no
// resolver is set, or the secret can't be resolved — callers log this
// rather than fail the request outright, since a misconfigured host is a
// config problem, not grounds to crash the gateway.
//
// Inject is a thin wrapper over Resolve. Callers that only need to know
// whether resolution would succeed — without holding an outbound request to
// modify — should call Resolve directly.
func (c *CredentialProvider) Inject(req *http.Request, host, namespace string) error {
	username, token, err := c.Resolve(host, namespace)
	if err != nil {
		return err
	}
	req.SetBasicAuth(username, token)
	return nil
}
