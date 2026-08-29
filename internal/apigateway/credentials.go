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
// wrappers implemented in PR1; oauth2 (PR2) resolves its access token
// through an OAuth2AccessTokenSource (oauth2.go) instead of the static
// resolver — see CredentialProvider.SetOAuth2TokenSource.
type AuthKind string

const (
	AuthBearer AuthKind = "bearer"
	AuthBasic  AuthKind = "basic"
	AuthHeader AuthKind = "header"
	AuthQuery  AuthKind = "query"
	// AuthOAuth2 injects `Authorization: Bearer <access_token>`, where
	// access_token is supplied by the CredentialProvider's
	// OAuth2AccessTokenSource (docs/plans/api-gateway.md §6/PR2). A
	// CredentialProvider constructed without SetOAuth2TokenSource ever
	// being called (every call site before PR2, and any test that doesn't
	// opt in) has no TokenSource wired, and Resolve/Inject both return a
	// clear "no OAuth2 TokenSource configured" error for it rather than
	// panicking or silently no-op'ing — a service misconfigured with kind:
	// oauth2 in that state fails a request with a 502 (see
	// Server.ServeHTTP), not a build/startup error and not a silent bypass.
	AuthOAuth2 AuthKind = "oauth2"
)

// ServiceAuth is the resolved (post config-validation) shape of one
// service's auth block. Which fields are meaningful depends on Kind:
//   - bearer: SecretKey
//   - basic:  Username, SecretKey
//   - header: Header, SecretKey
//   - query:  Query, SecretKey
//   - oauth2: Provider (names a CredentialProvider's wired OAuth2AccessTokenSource
//     provider entry; SecretKey/Username/Header/Query unused)
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
	// AllowReadOnlyWrite opts this ONE service out of the ordinary
	// read-only→GET/HEAD-only gate (Server.ServeHTTP, isSafeMethod).
	// docs/plans/api-gateway.md §論点 (2026-08-14 追加決定): readonly job
	// tokens still can't touch the sandbox filesystem, but some services
	// (e.g. a Slack "post completion report" webhook) are safe to let a
	// read-only job POST to even though the job can't write code. This is
	// deliberately a daemon-config-only knob — never project.yaml/
	// task_behaviors — because granting it from inside the repo would let a
	// prompt-injected agent grant itself write access to any service it can
	// already reach (docs/plans/api-gateway.md:59-62's "project.yaml には
	// credential アクセス権限を置かない" decision). Defaults to false
	// (fail-closed): a service must be explicitly opted in by whoever
	// controls config.yaml.
	AllowReadOnlyWrite bool
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
	auth               ServiceAuth
	baseURL            *url.URL
	allowReadOnlyWrite bool
}

// CredentialProvider knows which services the gateway can reach (name →
// base_url + auth config) and how to resolve each one's secret. Mirrors
// internal/gitgateway.CredentialProvider's role, generalized from a single
// Basic-auth shape to the four static AuthKind variants plus oauth2
// (docs/plans/api-gateway.md §4/§6, PR2) — the static kinds resolve through
// resolver; oauth2 resolves through oauth (SetOAuth2TokenSource).
type CredentialProvider struct {
	services map[string]resolvedService
	resolver SecretResolver
	oauth    OAuth2AccessTokenSource
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
		m[s.Name] = resolvedService{auth: s.Auth, baseURL: u, allowReadOnlyWrite: s.AllowReadOnlyWrite}
	}
	return &CredentialProvider{services: m, resolver: resolver}
}

// SetOAuth2TokenSource wires the OAuth2 access-token supplier oauth2-kind
// services resolve through (docs/plans/api-gateway.md §6, PR2). A
// CredentialProvider built via NewCredentialProvider alone — every call
// site before this PR, and any test that doesn't opt in — has oauth == nil,
// which keeps the pre-PR2 CONTRACT (not the exact error text — that changed
// to name SetOAuth2TokenSource explicitly): Resolve/Inject still always
// return a non-nil error for oauth2 in that state, never silently bypass or
// panic on a nil deref. A separate setter (rather than a new
// NewCredentialProvider parameter) avoids touching every existing call
// site: production wiring (internal/server/wire.go) calls this once, right
// after construction, and every apigateway/server test that doesn't care
// about oauth2 is unaffected.
func (c *CredentialProvider) SetOAuth2TokenSource(oauth OAuth2AccessTokenSource) {
	if c == nil {
		return
	}
	c.oauth = oauth
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

// OAuth2ProviderFor resolves service to the oauth_providers.<name> its
// auth.kind: oauth2 config references — the service->provider lookup `boid
// secret oauth login <service>` (PR3, docs/plans/api-gateway.md §7) needs
// to hand LoginManager.StartLogin the provider name it actually operates on
// (login.go's own methods take a provider, never a service — see
// LoginManager's own doc comment). ok is false when service is unknown OR
// is configured with any auth.kind OTHER than oauth2 — `boid secret oauth
// login` has nothing useful to do for a bearer/basic/header/query service
// (there is no OAuth2 grant to obtain).
func (c *CredentialProvider) OAuth2ProviderFor(service string) (provider string, ok bool) {
	if c == nil {
		return "", false
	}
	rs, ok := c.services[service]
	if !ok || rs.auth.Kind != AuthOAuth2 {
		return "", false
	}
	return rs.auth.Provider, true
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

// AllowsReadOnlyWrite reports whether name's ServiceConfig opted into the
// read-only-write escape hatch (ServiceConfig.AllowReadOnlyWrite's own doc
// comment). An unknown service (or a nil CredentialProvider) reports false —
// fail-closed, matching KnowsService/BaseURLFor's own posture for an unknown
// name.
func (c *CredentialProvider) AllowsReadOnlyWrite(name string) bool {
	if c == nil {
		return false
	}
	rs, ok := c.services[name]
	return ok && rs.allowReadOnlyWrite
}

// accountSecretKey returns the SecretStore key a static auth kind (bearer/
// basic/header/query) resolves against for account: base unmodified when
// account is empty — byte-identical to the pre-account-support key (docs/
// plans/api-gateway-credential-accounts.md D2) — or "<base>@<account>" when
// an account is specified (§"credential identity"). This is the ONE place
// PR-1 builds that composed key; PR-2 introduces the credentialID type to
// give oauth2's singleflight/memCache keys the same single-construction-site
// property once a second call site (AccessToken) needs the identical
// composition — a single call site here does not yet justify that type.
func accountSecretKey(base, account string) string {
	if account == "" {
		return base
	}
	return base + "@" + account
}

// Resolve validates that credential injection for name/account would
// succeed — without mutating any request — resolving the underlying secret
// (for the static kinds) so a bad/missing secret-store entry surfaces here
// rather than only once Inject is called on a live outbound request. This is
// the "pre-check" half of the fail-fast pattern
// docs/plans/gitgateway-credential-fail-fast.md established for the sibling
// git gateway: Server.ServeHTTP calls Resolve before proxying so it can 502
// without ever contacting the upstream when credentials fail to resolve.
//
// account is the optional credential-account qualifier (docs/plans/
// api-gateway-credential-accounts.md). For the static kinds it composes the
// secret-store key (accountSecretKey) — D3: there is no fallback to the
// account-less key when the qualified one is missing, so a non-empty account
// whose secret was never set fails here exactly like any other missing
// secret. oauth2 with a non-empty account is rejected outright (PR-2 scope)
// — see the AuthOAuth2 case below.
func (c *CredentialProvider) Resolve(name, account, namespace string) error {
	if c == nil {
		return fmt.Errorf("apigateway: no credential provider configured")
	}
	rs, ok := c.services[name]
	if !ok {
		return fmt.Errorf("apigateway: service %q is not configured", name)
	}
	switch rs.auth.Kind {
	case AuthOAuth2:
		if account != "" {
			return fmt.Errorf("apigateway: service %q: account-qualified credentials (%q) are not supported for auth.kind %q yet — PR-2 で対応予定 (docs/plans/api-gateway-credential-accounts.md)", name, account, AuthOAuth2)
		}
		if c.oauth == nil {
			return fmt.Errorf("apigateway: service %q uses auth kind %q, but no OAuth2 TokenSource is configured (docs/plans/api-gateway.md PR2 — SetOAuth2TokenSource)", name, AuthOAuth2)
		}
		if _, err := c.oauth.AccessToken(namespace, rs.auth.Provider); err != nil {
			return fmt.Errorf("apigateway: oauth2 access token for service %q (provider %q, namespace %q): %w", name, rs.auth.Provider, namespace, err)
		}
		return nil
	case AuthBearer, AuthBasic, AuthHeader, AuthQuery:
		if c.resolver == nil {
			return fmt.Errorf("apigateway: no secret resolver configured for service %q", name)
		}
		key := accountSecretKey(rs.auth.SecretKey, account)
		if _, err := c.resolver(namespace, key); err != nil {
			return fmt.Errorf("apigateway: resolve secret %q for service %q (namespace %q): %w", key, name, namespace, err)
		}
		return nil
	default:
		return fmt.Errorf("apigateway: service %q has unrecognized auth kind %q", name, rs.auth.Kind)
	}
}

// Inject resolves name/account's configured secret — scoped to namespace —
// and applies it to req according to the service's AuthKind. It returns an
// error (and leaves req unmodified) on any failure: unknown service, no
// resolver configured, a secret-store lookup error, or the reserved oauth2
// kind. Inject re-resolves the secret rather than reusing a prior Resolve
// call's result — see internal/gitgateway.CredentialProvider.Inject's own
// doc comment for why this thin-wrapper-over-Resolve shape is deliberate
// (Server.ServeHTTP's Resolve pre-check and this Inject call are two
// independent SecretStore reads, cheap in the common case).
//
// account behaves exactly as documented on Resolve — same accountSecretKey
// composition for the static kinds, same fail-closed oauth2 rejection when
// non-empty.
func (c *CredentialProvider) Inject(req *http.Request, name, account, namespace string) error {
	if c == nil {
		return fmt.Errorf("apigateway: no credential provider configured")
	}
	rs, ok := c.services[name]
	if !ok {
		return fmt.Errorf("apigateway: service %q is not configured", name)
	}
	switch rs.auth.Kind {
	case AuthOAuth2:
		if account != "" {
			return fmt.Errorf("apigateway: service %q: account-qualified credentials (%q) are not supported for auth.kind %q yet — PR-2 で対応予定 (docs/plans/api-gateway-credential-accounts.md)", name, account, AuthOAuth2)
		}
		if c.oauth == nil {
			return fmt.Errorf("apigateway: service %q uses auth kind %q, but no OAuth2 TokenSource is configured (docs/plans/api-gateway.md PR2 — SetOAuth2TokenSource)", name, AuthOAuth2)
		}
		token, err := c.oauth.AccessToken(namespace, rs.auth.Provider)
		if err != nil {
			return fmt.Errorf("apigateway: oauth2 access token for service %q (provider %q, namespace %q): %w", name, rs.auth.Provider, namespace, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	case AuthBearer, AuthBasic, AuthHeader, AuthQuery:
	default:
		return fmt.Errorf("apigateway: service %q has unrecognized auth kind %q", name, rs.auth.Kind)
	}
	if c.resolver == nil {
		return fmt.Errorf("apigateway: no secret resolver configured for service %q", name)
	}
	key := accountSecretKey(rs.auth.SecretKey, account)
	secret, err := c.resolver(namespace, key)
	if err != nil {
		return fmt.Errorf("apigateway: resolve secret %q for service %q (namespace %q): %w", key, name, namespace, err)
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
