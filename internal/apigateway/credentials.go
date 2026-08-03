package apigateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

// AuthKind selects how the gateway injects credentials into a proxied
// request. docs/plans/api-gateway.md §4 ("credential 注入: CredentialProvider
// → TokenSource 一般化"): bearer/basic/header/query are static SecretStore
// wrappers implemented in this PR; oauth2 is reserved (config schema only —
// PR2 wires the actual TokenSource, docs/plans/api-gateway.md PR 分割案 PR2).
type AuthKind string

const (
	AuthBearer AuthKind = "bearer"
	AuthBasic  AuthKind = "basic"
	AuthHeader AuthKind = "header"
	AuthQuery  AuthKind = "query"
	// AuthOAuth2 is parsed and validated (config schema reservation only —
	// docs/plans/api-gateway.md PR1 scope note: "oauth2 kindはPR2で足すので
	// 今回はconfig schemaにoauth2を予約する程度でよい"). Resolve/Inject both
	// return a clear "not implemented yet" error for it rather than
	// panicking or silently no-op'ing, so a service misconfigured with
	// kind: oauth2 today fails a request with a 502 (see Server.ServeHTTP),
	// not a build/startup error and not a silent bypass.
	AuthOAuth2 AuthKind = "oauth2"
)

// ServiceAuth is the resolved (post config-validation) shape of one
// service's auth block. Which fields are meaningful depends on Kind:
//   - bearer: SecretKey
//   - basic:  Username, SecretKey
//   - header: Header, SecretKey
//   - query:  Query, SecretKey
//   - oauth2: Provider (reserved; SecretKey/Username/Header/Query unused)
type ServiceAuth struct {
	Kind      AuthKind
	SecretKey string
	Username  string
	Header    string
	Query     string
	Provider  string
}

// ServiceConfig declares one logical service the gateway can proxy to:
// its upstream base URL and how to authenticate requests to it.
// docs/plans/api-gateway.md §2 ("service registry (config.yaml)").
type ServiceConfig struct {
	Name    string
	BaseURL string
	Auth    ServiceAuth
}

// SecretResolver resolves a secret-store key reference, scoped to a
// namespace, to its plaintext value — the same shape as
// internal/gitgateway.SecretResolver (see that type's own doc comment for
// why this is a plain function type rather than an internal/dispatcher
// import: it keeps this package free of the sqlite-backed internal/db
// build). An empty namespace is expected to fall back to a "default"
// namespace, mirroring internal/dispatcher.SecretStore.Get's own
// normalizeNamespace.
type SecretResolver func(namespace, key string) (string, error)

// resolvedService is a ServiceConfig with its base_url pre-parsed once at
// construction time, so per-request handling never re-parses it.
type resolvedService struct {
	auth    ServiceAuth
	baseURL *url.URL
}

// CredentialProvider knows which services the gateway can reach (name →
// base_url + auth config) and how to resolve each one's secret. Mirrors
// internal/gitgateway.CredentialProvider's role, generalized from a single
// Basic-auth shape to the four static AuthKind variants (plus the reserved
// oauth2 kind) — docs/plans/api-gateway.md §4.
type CredentialProvider struct {
	services map[string]resolvedService
	resolver SecretResolver
}

// NewCredentialProvider builds a CredentialProvider from the daemon's
// configured services and the resolver used to fetch each service's secret
// value. A service whose base_url fails to parse, or parses without both a
// scheme and a host, is skipped with a warning rather than aborting
// construction — config.yaml validation (internal/config) is expected to
// have already rejected this at load time, so reaching this point is
// defensive-only, matching internal/config.GatewayConfig.HostConfigs's own
// "already validated" invariant note.
func NewCredentialProvider(services []ServiceConfig, resolver SecretResolver) *CredentialProvider {
	m := make(map[string]resolvedService, len(services))
	for _, s := range services {
		u, err := url.Parse(s.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			slog.Warn("apigateway: service has an invalid base_url; skipping",
				"service", s.Name, "base_url", s.BaseURL, "error", err)
			continue
		}
		m[s.Name] = resolvedService{auth: s.Auth, baseURL: u}
	}
	return &CredentialProvider{services: m, resolver: resolver}
}

// Configured reports whether c has any secret resolver wired at all —
// distinguishing a systemic "no secret store configured" state (every
// request would fail identically) from an ordinary per-key miss on an
// otherwise-healthy store. Mirrors internal/gitgateway.CredentialProvider.Configured.
func (c *CredentialProvider) Configured() bool {
	return c != nil && c.resolver != nil
}

// KnowsService reports whether c has a ServiceConfig entry for name — i.e.
// whether name is in the gateway's configured service registry. A nil
// CredentialProvider knows no services (fail-closed).
func (c *CredentialProvider) KnowsService(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.services[name]
	return ok
}

// BaseURLFor returns the parsed upstream base URL for name, or ok=false when
// name is unknown (or c is nil).
func (c *CredentialProvider) BaseURLFor(name string) (u *url.URL, ok bool) {
	if c == nil {
		return nil, false
	}
	rs, ok := c.services[name]
	if !ok {
		return nil, false
	}
	return rs.baseURL, true
}

// Resolve validates that credential injection for name would succeed —
// without mutating any request — resolving the underlying secret (for the
// static kinds) so a bad/missing secret-store entry surfaces here rather
// than only once Inject is called on a live outbound request. This is the
// "pre-check" half of the fail-fast pattern
// docs/plans/gitgateway-credential-fail-fast.md established for the sibling
// git gateway: Server.ServeHTTP calls Resolve before proxying so it can 502
// without ever contacting the upstream when credentials fail to resolve.
func (c *CredentialProvider) Resolve(name, namespace string) error {
	if c == nil {
		return fmt.Errorf("apigateway: no credential provider configured")
	}
	rs, ok := c.services[name]
	if !ok {
		return fmt.Errorf("apigateway: service %q is not configured", name)
	}
	switch rs.auth.Kind {
	case AuthOAuth2:
		return fmt.Errorf("apigateway: service %q uses auth kind %q, which is not implemented yet (docs/plans/api-gateway.md PR2)", name, AuthOAuth2)
	case AuthBearer, AuthBasic, AuthHeader, AuthQuery:
		if c.resolver == nil {
			return fmt.Errorf("apigateway: no secret resolver configured for service %q", name)
		}
		if _, err := c.resolver(namespace, rs.auth.SecretKey); err != nil {
			return fmt.Errorf("apigateway: resolve secret %q for service %q (namespace %q): %w", rs.auth.SecretKey, name, namespace, err)
		}
		return nil
	default:
		return fmt.Errorf("apigateway: service %q has unrecognized auth kind %q", name, rs.auth.Kind)
	}
}

// Inject resolves name's configured secret — scoped to namespace — and
// applies it to req according to the service's AuthKind. It returns an
// error (and leaves req unmodified) on any failure: unknown service, no
// resolver configured, a secret-store lookup error, or the reserved oauth2
// kind. Inject re-resolves the secret rather than reusing a prior Resolve
// call's result — see internal/gitgateway.CredentialProvider.Inject's own
// doc comment for why this thin-wrapper-over-Resolve shape is deliberate
// (Server.ServeHTTP's Resolve pre-check and this Inject call are two
// independent SecretStore reads, cheap in the common case).
func (c *CredentialProvider) Inject(req *http.Request, name, namespace string) error {
	if c == nil {
		return fmt.Errorf("apigateway: no credential provider configured")
	}
	rs, ok := c.services[name]
	if !ok {
		return fmt.Errorf("apigateway: service %q is not configured", name)
	}
	switch rs.auth.Kind {
	case AuthOAuth2:
		return fmt.Errorf("apigateway: service %q uses auth kind %q, which is not implemented yet (docs/plans/api-gateway.md PR2)", name, AuthOAuth2)
	case AuthBearer, AuthBasic, AuthHeader, AuthQuery:
	default:
		return fmt.Errorf("apigateway: service %q has unrecognized auth kind %q", name, rs.auth.Kind)
	}
	if c.resolver == nil {
		return fmt.Errorf("apigateway: no secret resolver configured for service %q", name)
	}
	secret, err := c.resolver(namespace, rs.auth.SecretKey)
	if err != nil {
		return fmt.Errorf("apigateway: resolve secret %q for service %q (namespace %q): %w", rs.auth.SecretKey, name, namespace, err)
	}
	switch rs.auth.Kind {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+secret)
	case AuthBasic:
		req.SetBasicAuth(rs.auth.Username, secret)
	case AuthHeader:
		req.Header.Set(rs.auth.Header, secret)
	case AuthQuery:
		q := req.URL.Query()
		q.Set(rs.auth.Query, secret)
		req.URL.RawQuery = q.Encode()
	}
	return nil
}
