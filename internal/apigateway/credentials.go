package apigateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// AuthKind selects how the gateway injects credentials into a proxied
// request. bearer/basic/header/query are static SecretStore wrappers;
// oauth2 resolves its access token through an OAuth2AccessTokenSource
// instead of the static resolver — see CredentialProvider.SetOAuth2TokenSource.
type AuthKind string

const (
	AuthBearer AuthKind = "bearer"
	AuthBasic  AuthKind = "basic"
	AuthHeader AuthKind = "header"
	AuthQuery  AuthKind = "query"
	// AuthOAuth2 injects `Authorization: Bearer <access_token>`, where
	// access_token is supplied by the CredentialProvider's
	// OAuth2AccessTokenSource. A CredentialProvider on which
	// SetOAuth2TokenSource was never called has no TokenSource wired, and
	// Resolve/Inject both return a clear "no OAuth2 TokenSource configured"
	// error for it rather than panicking or silently no-op'ing — a service
	// misconfigured with kind: oauth2 in that state fails a request with a
	// 502 (see Server.ServeHTTP), not a build/startup error and not a
	// silent bypass.
	AuthOAuth2 AuthKind = "oauth2"
)

// ServiceAuth is the resolved (post config-validation) shape of one
// service's auth block. Which fields are meaningful depends on Kind:
//   - bearer: SecretKey
//   - basic:  Username or UsernameSecretKey, SecretKey
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
	// UsernameSecretKey, when non-empty (kind: basic only), resolves the
	// Basic-auth username from the secret store instead of using the
	// literal Username field — account-qualified via the same
	// accountSecretKey composition SecretKey already uses. Mutually
	// exclusive with Username; config.validateServiceConfig enforces that
	// at load time. Motivating case: a Jira-Cloud-shaped service where the
	// login email differs per tenant/account, not just the token.
	UsernameSecretKey string
}

// ServiceConfig declares one logical service the gateway can proxy to:
// its upstream base URL and how to authenticate requests to it.
type ServiceConfig struct {
	Name    string
	BaseURL string
	Auth    ServiceAuth
	// BaseURLSecretKey, when non-empty, resolves BaseURL from the secret
	// store instead of using the literal BaseURL field — account-qualified
	// via the same accountSecretKey composition auth.SecretKey already
	// uses. Mutually exclusive with BaseURL; config.validateServiceConfig
	// enforces that at load time. Motivating case: Jira Cloud, where the
	// whole upstream tenant (subdomain) differs per account, not just the
	// credential.
	BaseURLSecretKey string
	// AllowInsecure mirrors config.ServiceConfig.AllowInsecure — needed
	// here because a BaseURLSecretKey-backed base_url can only be
	// validated (https-unless-allowed, absolute URL, no query/fragment) at
	// REQUEST time, once the secret store value is known, never at
	// config-load time the way a literal BaseURL is. Unused when
	// BaseURLSecretKey is empty — the literal BaseURL path was already
	// validated by config.validateServiceConfig before this ServiceConfig
	// was ever built.
	AllowInsecure bool
	// AllowReadOnlyWrite opts this ONE service out of the ordinary
	// read-only→GET/HEAD-only gate (Server.ServeHTTP, isSafeMethod):
	// readonly job tokens still can't touch the sandbox filesystem, but
	// some services (e.g. a Slack "post completion report" webhook) are
	// safe to let a read-only job POST to even though the job can't write
	// code. This is deliberately a daemon-config-only knob — never
	// project.yaml/task_behaviors — because granting it from inside the
	// repo would let a prompt-injected agent grant itself write access to
	// any service it can already reach. Defaults to false (fail-closed): a
	// service must be explicitly opted in by whoever controls config.yaml.
	AllowReadOnlyWrite bool
	// RequireAccount is this ServiceConfig's post-validation mirror of
	// config.ServiceConfig.RequireAccount (see that field's own doc comment
	// for the full rationale) — the same relationship AllowReadOnlyWrite
	// above already has to its own config counterpart.
	RequireAccount bool
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
// construction time, so per-request handling never re-parses it — UNLESS
// baseURLSecretKey is set, in which case baseURL is nil and the real URL
// is resolved lazily, per (namespace, account), on every BaseURLFor call:
// there is no literal value to pre-parse until the secret store is
// consulted. Exactly one of baseURL/baseURLSecretKey is set for any
// resolvedService in the map.
type resolvedService struct {
	auth             ServiceAuth
	baseURL          *url.URL
	baseURLSecretKey string
	// allowInsecure is only consulted when baseURLSecretKey is set — see
	// BaseURLFor. A literal baseURL was already https-validated (or
	// explicitly allowed not to be) by config.validateServiceConfig before
	// NewCredentialProvider ever ran.
	allowInsecure      bool
	allowReadOnlyWrite bool
	requireAccount     bool
}

// CredentialProvider knows which services the gateway can reach (name →
// base_url + auth config) and how to resolve each one's secret. The static
// AuthKind variants resolve through resolver; oauth2 resolves through
// oauth (SetOAuth2TokenSource).
type CredentialProvider struct {
	services map[string]resolvedService
	resolver SecretResolver
	oauth    OAuth2AccessTokenSource
}

// NewCredentialProvider builds a CredentialProvider from the daemon's
// configured services and the resolver used to fetch each service's secret
// value. A service whose LITERAL base_url fails to parse, or parses without
// both a scheme and a host, is skipped with a warning rather than aborting
// construction — config.yaml validation (internal/config) is expected to
// have already rejected this at load time, so reaching this point is
// defensive-only. This "already validated by config load" invariant does
// NOT extend to a BaseURLSecretKey-backed service: that service's real
// base_url string does not exist yet at config-load time (it lives in the
// secret store, account-qualified), so such a service is always admitted
// into the map in secret-backed mode, and BaseURLFor validates the
// resolved value at request time instead.
func NewCredentialProvider(services []ServiceConfig, resolver SecretResolver) *CredentialProvider {
	m := make(map[string]resolvedService, len(services))
	for _, s := range services {
		if s.BaseURLSecretKey != "" {
			m[s.Name] = resolvedService{
				auth:               s.Auth,
				baseURLSecretKey:   s.BaseURLSecretKey,
				allowInsecure:      s.AllowInsecure,
				allowReadOnlyWrite: s.AllowReadOnlyWrite,
				requireAccount:     s.RequireAccount,
			}
			continue
		}
		u, err := url.Parse(s.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			slog.Warn("apigateway: service has an invalid base_url; skipping",
				"service", s.Name, "base_url", s.BaseURL, "error", err)
			continue
		}
		m[s.Name] = resolvedService{auth: s.Auth, baseURL: u, allowReadOnlyWrite: s.AllowReadOnlyWrite, requireAccount: s.RequireAccount}
	}
	return &CredentialProvider{services: m, resolver: resolver}
}

// SetOAuth2TokenSource wires the OAuth2 access-token supplier oauth2-kind
// services resolve through. A CredentialProvider on which this is never
// called has oauth == nil: Resolve/Inject still always return a non-nil
// error for oauth2 in that state, never silently bypass or panic on a nil
// deref. A separate setter (rather than a NewCredentialProvider parameter)
// avoids touching every existing call site.
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
// auth.kind: oauth2 config references — `boid secret oauth login <service>`
// needs this to hand LoginManager.StartLogin the provider name it actually
// operates on (login.go's own methods take a provider, never a service).
// ok is false when service is unknown OR is configured with any auth.kind
// OTHER than oauth2 — there is no OAuth2 grant to obtain for those.
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

// BaseURLFor returns the upstream base URL for (name, account). For a
// LITERAL base_url service this is the pre-parsed value from
// NewCredentialProvider — a zero-cost lookup, no resolver call. For a
// BaseURLSecretKey-backed service it resolves "<key>@<account>" (or the
// bare key when account is empty) from the secret store, scoped to
// namespace, and validates the result with the same rules
// config.ValidateServiceURL enforces at config-load time for a literal
// base_url (ValidateBaseURL — the shared implementation both call): an
// absolute URL, no query/fragment, https unless the service's
// AllowInsecure opted out. namespace/account are therefore ignored
// entirely for a literal-base_url service.
//
// Returns an error — never a bare ok=false — because a secret-backed
// resolution has real failure modes (missing secret, malformed URL value)
// that deserve a specific message, the same shape Resolve/Inject already
// use for credential resolution. Server.ServeHTTP logs the full error
// server-side but must never echo it (or the resolved URL) to the sandbox —
// see that call site's own comment.
func (c *CredentialProvider) BaseURLFor(namespace, name, account string) (*url.URL, error) {
	if c == nil {
		return nil, fmt.Errorf("apigateway: no credential provider configured")
	}
	rs, ok := c.services[name]
	if !ok {
		return nil, fmt.Errorf("apigateway: service %q is not configured", name)
	}
	if rs.baseURL != nil {
		return rs.baseURL, nil
	}
	if c.resolver == nil {
		return nil, fmt.Errorf("apigateway: no secret resolver configured for service %q", name)
	}
	key := accountSecretKey(rs.baseURLSecretKey, account)
	raw, err := c.resolver(namespace, key)
	if err != nil {
		return nil, fmt.Errorf("apigateway: resolve base_url secret %q for service %q (namespace %q): %w", key, name, namespace, err)
	}
	if err := ValidateBaseURL(name, "base_url", raw, rs.allowInsecure); err != nil {
		return nil, fmt.Errorf("apigateway: resolved base_url for service %q (namespace %q, account %q) failed validation: %w", name, namespace, account, err)
	}
	// ValidateBaseURL already proved raw parses cleanly with a scheme and
	// host — this second Parse cannot fail in practice; err is checked
	// anyway rather than assumed away, matching this file's own posture on
	// "should be unreachable" paths elsewhere (server.go's upstreamURL
	// parse has the identical shape).
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("apigateway: resolved base_url for service %q parsed cleanly during validation but failed on re-parse: %w", name, err)
	}
	return u, nil
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

// RequiresAccount reports whether name's ServiceConfig opted into rejecting
// account-less requests (ServiceConfig.RequireAccount's own doc comment).
// An unknown service (or a nil CredentialProvider) reports false.
//
// Unlike AllowsReadOnlyWrite, false here is not a fail-closed default but a
// deliberate choice to defer to BaseURLFor's diagnosis: a name can clear
// the workspace authorization check yet still be absent from c.services
// (the workspace's enabled-services list and config.yaml's services: map
// can drift). For such a name, BaseURLFor already returns "service %q is
// not configured" and Server.ServeHTTP reports that 502 — reporting 400
// "account required" instead, before that check runs, would be
// misleading. RequiresAccount only ever has an opinion once a service
// definitely exists.
func (c *CredentialProvider) RequiresAccount(name string) bool {
	if c == nil {
		return false
	}
	rs, ok := c.services[name]
	return ok && rs.requireAccount
}

// accountSecretKey returns the SecretStore key a static auth kind (bearer/
// basic/header/query) resolves against for account: base unmodified when
// account is empty, or "<base>@<account>" when an account is specified.
func accountSecretKey(base, account string) string {
	if account == "" {
		return base
	}
	return base + "@" + account
}

// Resolve validates that credential injection for name/account would
// succeed — without mutating any request — resolving the underlying secret
// (for the static kinds) so a bad/missing secret-store entry surfaces here
// rather than only once Inject is called on a live outbound request. This
// is the "pre-check" half of a fail-fast pattern: Server.ServeHTTP calls
// Resolve before proxying so it can 502 without ever contacting the
// upstream when credentials fail to resolve.
//
// account is the optional credential-account qualifier. For the static
// kinds it composes the secret-store key (accountSecretKey): there is no
// fallback to the account-less key when the qualified one is missing, so a
// non-empty account whose secret was never set fails here exactly like any
// other missing secret. For oauth2 it is folded into a credentialID and
// handed to c.oauth.AccessToken, which applies the identical no-fallback
// contract.
//
// namespace is deliberately the FIRST parameter, not adjacent to the other
// two string args: with three plain strings in a row, a call site that
// accidentally writes them in the wrong order still compiles, and
// workspace-scoped credential isolation depends on namespace never being
// mixed up with account or name (a wrong namespace resolves a DIFFERENT
// workspace's secret). This also matches this package's own SecretResolver
// convention (namespace, key).
func (c *CredentialProvider) Resolve(namespace, name, account string) error {
	if c == nil {
		return fmt.Errorf("apigateway: no credential provider configured")
	}
	rs, ok := c.services[name]
	if !ok {
		return fmt.Errorf("apigateway: service %q is not configured", name)
	}
	switch rs.auth.Kind {
	case AuthOAuth2:
		if c.oauth == nil {
			return fmt.Errorf("apigateway: service %q uses auth kind %q, but no OAuth2 TokenSource is configured (docs/plans/api-gateway.md PR2 — SetOAuth2TokenSource)", name, AuthOAuth2)
		}
		// This is the ONLY place in this file a credentialID is
		// constructed for an oauth2 request; c.oauth.AccessToken is the
		// only thing that reads it.
		cred := credentialID{provider: rs.auth.Provider, account: account}
		if _, err := c.oauth.AccessToken(namespace, cred); err != nil {
			return fmt.Errorf("apigateway: oauth2 access token for service %q (provider %q, account %q, namespace %q): %w", name, rs.auth.Provider, account, namespace, err)
		}
		return nil
	case AuthBearer, AuthBasic, AuthHeader, AuthQuery:
		if c.resolver == nil {
			return fmt.Errorf("apigateway: no secret resolver configured for service %q", name)
		}
		// A UsernameSecretKey-backed username has to resolve (and pass
		// the RFC 7617 colon check) here too, not just in Inject — this
		// is the fail-fast pre-check Server.ServeHTTP relies on to 502
		// before ever proxying.
		if rs.auth.Kind == AuthBasic {
			if _, err := c.resolveUsername(namespace, name, account, rs); err != nil {
				return err
			}
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

// resolveUsername returns the Basic-auth username to inject for rs: the
// literal rs.auth.Username when UsernameSecretKey is empty (no resolver
// call), or the account-qualified (accountSecretKey — the same composition
// SecretKey uses) secret-store value otherwise.
//
// The RFC 7617 §2 "must not contain a colon" check
// config.validateServiceConfig applies at config-load time for a literal
// Username (a Basic credential is built as "username:password", split on
// the FIRST colon, so a colon in the username changes what the upstream
// parses as each half) is re-applied here for a resolved secret-store
// value, which cannot be checked until this point — net/http's
// SetBasicAuth has no validation of its own, it just base64-encodes
// verbatim.
func (c *CredentialProvider) resolveUsername(namespace, name, account string, rs resolvedService) (string, error) {
	if rs.auth.UsernameSecretKey == "" {
		return rs.auth.Username, nil
	}
	if c.resolver == nil {
		return "", fmt.Errorf("apigateway: no secret resolver configured for service %q", name)
	}
	key := accountSecretKey(rs.auth.UsernameSecretKey, account)
	username, err := c.resolver(namespace, key)
	if err != nil {
		return "", fmt.Errorf("apigateway: resolve username secret %q for service %q (namespace %q): %w", key, name, namespace, err)
	}
	if strings.Contains(username, ":") {
		return "", fmt.Errorf("apigateway: resolved auth.username_secret_key value for service %q must not contain \":\" (RFC 7617 §2 — the Basic credential is built as \"username:secret\", so a colon in the username changes what the upstream parses as each half)", name)
	}
	return username, nil
}

// Inject resolves name/account's configured secret — scoped to namespace —
// and applies it to req according to the service's AuthKind. It returns an
// error (and leaves req unmodified) on any failure: unknown service, no
// resolver configured, a secret-store lookup error, or the reserved oauth2
// kind. Inject re-resolves the secret rather than reusing a prior Resolve
// call's result — Server.ServeHTTP's Resolve pre-check and this Inject
// call are two independent SecretStore reads, cheap in the common case.
//
// account behaves exactly as documented on Resolve — same accountSecretKey
// composition for the static kinds, same credentialID composition for
// oauth2. namespace is the first parameter for the same reason documented
// on Resolve.
func (c *CredentialProvider) Inject(req *http.Request, namespace, name, account string) error {
	if c == nil {
		return fmt.Errorf("apigateway: no credential provider configured")
	}
	rs, ok := c.services[name]
	if !ok {
		return fmt.Errorf("apigateway: service %q is not configured", name)
	}
	switch rs.auth.Kind {
	case AuthOAuth2:
		if c.oauth == nil {
			return fmt.Errorf("apigateway: service %q uses auth kind %q, but no OAuth2 TokenSource is configured (docs/plans/api-gateway.md PR2 — SetOAuth2TokenSource)", name, AuthOAuth2)
		}
		// See Resolve's identical credentialID construction comment.
		cred := credentialID{provider: rs.auth.Provider, account: account}
		token, err := c.oauth.AccessToken(namespace, cred)
		if err != nil {
			return fmt.Errorf("apigateway: oauth2 access token for service %q (provider %q, account %q, namespace %q): %w", name, rs.auth.Provider, account, namespace, err)
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
		username, err := c.resolveUsername(namespace, name, account, rs)
		if err != nil {
			return err
		}
		req.SetBasicAuth(username, secret)
	case AuthHeader:
		req.Header.Set(rs.auth.Header, secret)
	case AuthQuery:
		q := req.URL.Query()
		q.Set(rs.auth.Query, secret)
		req.URL.RawQuery = q.Encode()
	}
	return nil
}
