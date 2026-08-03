package config

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/apigateway"
)

// ServiceAuthConfig configures how the API gateway authenticates requests
// to one service. docs/plans/api-gateway.md §2 ("service registry
// (config.yaml)") / §4 ("credential 注入"). Which fields are required
// depends on Kind — see validateServiceConfig.
type ServiceAuthConfig struct {
	// Kind selects the injection strategy: "bearer" / "basic" / "header" /
	// "query" (implemented, PR1) or "oauth2" (reserved — schema only, PR2
	// adds the actual TokenSource; docs/plans/api-gateway.md PR1 scope note).
	Kind string `yaml:"kind"`
	// SecretKey is a reference into the secret store
	// (internal/dispatcher/secret_store.go); never a plaintext value.
	// Required for bearer/basic/header/query; unused for oauth2.
	SecretKey string `yaml:"secret_key,omitempty"`
	// Username is the Basic-auth username. Required (and only meaningful)
	// for kind: basic.
	Username string `yaml:"username,omitempty"`
	// Header is the header name to set. Required (and only meaningful) for
	// kind: header.
	Header string `yaml:"header,omitempty"`
	// Query is the query parameter name to set. Required (and only
	// meaningful) for kind: query.
	Query string `yaml:"query,omitempty"`
	// Provider names the OAuth2 provider (config.yaml oauth_providers:
	// block, PR2 — docs/plans/api-gateway.md §論点4). Required (and only
	// meaningful) for kind: oauth2; unused and unvalidated in PR1 beyond
	// "must be non-empty".
	Provider string `yaml:"provider,omitempty"`
}

// ServiceConfig declares one logical service the API gateway can proxy job
// requests to. docs/plans/api-gateway.md §2.
type ServiceConfig struct {
	// BaseURL is the upstream base URL (scheme + host, optionally a path
	// prefix) — never exposed to the sandbox; only the logical service name
	// in the route path is (docs/plans/api-gateway.md §1).
	BaseURL string            `yaml:"base_url"`
	Auth    ServiceAuthConfig `yaml:"auth"`
	// AllowInsecure must be explicitly set to true for BaseURL to use any
	// scheme other than "https" (codex review finding, round 4: a bare
	// warning-only posture is not fail-closed — an operator who never reads
	// boid.log has no signal that a credential is crossing the network in
	// cleartext). Required, not merely warned-about, so choosing a plaintext
	// upstream is always a conscious, config-visible decision — see
	// validateServiceConfig's own doc comment for why plaintext is still
	// SUPPORTED at all (PR1's stated primary use case: an internal test/ops
	// API that legitimately has no TLS yet).
	AllowInsecure bool `yaml:"allow_insecure,omitempty"`
}

// validServiceAuthKinds is the initial AuthKind set docs/plans/api-gateway.md
// §2 declares: "auth.kind の初期セット: bearer / basic / header / query /
// oauth2". oauth2 validates only that Provider is set — everything else
// about it is PR2's concern.
var validServiceAuthKinds = map[string]bool{
	string(apigateway.AuthBearer): true,
	string(apigateway.AuthBasic):  true,
	string(apigateway.AuthHeader): true,
	string(apigateway.AuthQuery):  true,
	string(apigateway.AuthOAuth2): true,
}

// validateServiceConfig validates one services.<name> entry, returning a
// descriptive error naming both the service and the missing/invalid field —
// the same "fail loud with the offending name" posture
// resolveForgeConfig/GatewayConfig's validation uses.
func validateServiceConfig(name string, sc ServiceConfig) error {
	// codex review finding: orchestrator.ResolveEnabledServices trims every
	// service NAME it resolves (floor entries and workspace Services list
	// entries alike) before matching, but the service REGISTRY built from
	// this config (apigateway.CredentialProvider's internal map, keyed
	// verbatim by this services.<name> map key) never did — so a service
	// declared as `services: {" myapp": ...}` (a quoted YAML key carrying
	// whitespace) would validate cleanly here yet never resolve for any
	// workspace, since ResolveEnabledServices's resolved name ("myapp",
	// trimmed) never matches the registry's untrimmed key (" myapp").
	// Rejecting a whitespace-padded name outright at config-load time is
	// simpler and more honest than trying to trim it silently (a silently
	// renamed service key is its own kind of surprise for `boid config get`/
	// `services_floor` cross-references) — an operator's `services:` map key
	// should already be name.
	if trimmed := strings.TrimSpace(name); trimmed != name {
		return fmt.Errorf("services[%q]: service name must not have leading/trailing whitespace (did you mean %q?)", name, trimmed)
	}
	// codex review round 7 finding: an empty service name (`services: {"":
	// ...}`) passed every check above (TrimSpace("") == "", so the
	// whitespace check doesn't catch it) yet apigateway.parsePath's route
	// segment for a service name can never be empty (it requires a
	// non-empty "/"-delimited path segment) — a service declared this way
	// would validate cleanly at config-load time but be permanently
	// unreachable from any sandbox request. Reject it explicitly instead of
	// relying on the whitespace check's coincidental (and confusing, if it
	// ever changed) side effect.
	if name == "" {
		return fmt.Errorf("services: a service name must not be empty")
	}
	if sc.BaseURL == "" {
		return fmt.Errorf("services[%q]: missing required \"base_url\" field", name)
	}
	// Parsed and scheme/host-checked here, at config-load time, rather than
	// left for apigateway.NewCredentialProvider's own defensive parse to
	// discover later (internal/server/wire.go). That constructor's doc
	// comment states an "already validated by config load" invariant for
	// exactly this reason — a malformed base_url should fail `boid start`
	// loudly (the same posture gateway.forges.*.host gets), not silently
	// vanish from the gateway's service registry with only a log warning
	// once the daemon is already running.
	u, err := url.Parse(sc.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("services[%q]: \"base_url\" must be an absolute URL with a scheme and host (got %q)", name, sc.BaseURL)
	}
	// A query string or fragment on base_url is rejected outright (codex
	// review round 2 finding), rather than silently ignored: apigateway.
	// Server always forwards the INBOUND request's own RawQuery verbatim
	// (docs/plans/api-gateway.md §5 — the sandbox's own query string must
	// reach the upstream unchanged) and never merges it with anything from
	// base_url, and a URL fragment is never sent to an HTTP server at all
	// (RFC 3986 §3.5 — it is a client-side-only construct). An operator
	// writing `base_url: https://host/api?tenant=x` expecting "?tenant=x"
	// to always be appended would otherwise see it silently vanish on
	// every single request with no indication why — failing the config
	// load instead directs them to the one place that DOES support a
	// fixed, always-injected value: the query AuthKind (a fixed query
	// parameter set from the secret store, not encoded into base_url).
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("services[%q]: \"base_url\" must not include a query string or fragment (got %q) — the sandbox's own query string is always forwarded as-is; use auth.kind: query for a fixed, injected query parameter instead", name, sc.BaseURL)
	}
	// docs/plans/api-gateway.md §1 states the gateway's own outbound leg as
	// "gateway → upstream は daemon 発の HTTPS" — a plain-http base_url means
	// this service's injected bearer/basic/header/query credential crosses
	// the network to the upstream in cleartext. Not rejected outright: PR1's
	// own stated primary use case ("自社開発アプリのテスト・運用API") often
	// means an internal test/staging environment that legitimately has no
	// TLS yet, and hard-rejecting http:// unconditionally would make the
	// gateway unusable for exactly that case.
	//
	// AllowInsecure IS required, though (codex review round 4 finding,
	// escalated from a round-2 warn-only version of this check): a bare log
	// warning is not fail-closed — an operator who never greps boid.log
	// (or whose daemon logs are quietly discarded/rotated away) has NO
	// signal that a credential is crossing the network in cleartext, and a
	// copy-pasted-from-an-insecure-example or typo'd "http" scheme would
	// otherwise ship silently. Requiring `allow_insecure: true` in the same
	// services.<name> block moves the acknowledgment into the config
	// document itself — visible in `boid config get`, in code review of a
	// config.yaml change, and in `boid config apply`'s diff — rather than a
	// log line that may never be read. This still does not block the real
	// use case: it becomes a one-line, explicit, self-documenting opt-in
	// instead of an unconditional accept.
	if u.Scheme != "https" && !sc.AllowInsecure {
		return fmt.Errorf(
			"services[%q]: \"base_url\" scheme %q is not https, so this service's injected credential would cross the network in cleartext — "+
				"set \"allow_insecure: true\" on this service if that is intentional (e.g. an internal test/staging API with no TLS yet)",
			name, u.Scheme)
	}
	// allow_insecure only ever bought a plaintext-HTTP escape hatch — never
	// license for an arbitrary scheme (codex review round 6 finding): the
	// gateway's outbound Transport is a fixed *http.Transport (server.go's
	// NewServer), which only ever speaks http/https regardless of this flag,
	// so "ftp"/"ws"/anything else would validate cleanly here yet fail every
	// single request at 502 once dispatched. Rejecting it at config-load
	// time turns that into an immediate, actionable `boid start` error
	// instead of a request-time surprise.
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("services[%q]: \"base_url\" scheme %q is not supported — the gateway only ever speaks http or https to the upstream, regardless of \"allow_insecure\"", name, u.Scheme)
	}
	if u.Scheme != "https" {
		slog.Warn("services: base_url does not use https — this service's injected credential will cross the network to the upstream in cleartext",
			"service", name, "base_url", sc.BaseURL, "scheme", u.Scheme)
	}
	if !validServiceAuthKinds[sc.Auth.Kind] {
		return fmt.Errorf("services[%q]: auth.kind: unrecognized %q (want one of bearer, basic, header, query, oauth2)", name, sc.Auth.Kind)
	}
	switch apigateway.AuthKind(sc.Auth.Kind) {
	case apigateway.AuthBearer:
		if sc.Auth.SecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind bearer requires \"secret_key\"", name)
		}
	case apigateway.AuthBasic:
		if sc.Auth.Username == "" {
			return fmt.Errorf("services[%q]: auth.kind basic requires \"username\"", name)
		}
		// RFC 7617 §2: the userid (username) component of a Basic
		// credential must not contain a colon — the credential is built as
		// "userid:password" and the FIRST colon is what separates the two,
		// so a username containing one changes what the upstream actually
		// parses as the username vs. the password (codex review round 6
		// finding). CredentialProvider.Inject's req.SetBasicAuth(username,
		// secret) has no validation of its own (net/http's SetBasicAuth
		// just base64-encodes "username:password" verbatim), so this would
		// otherwise pass config-load cleanly and only surface as a
		// confusing upstream 401 at request time.
		if strings.Contains(sc.Auth.Username, ":") {
			return fmt.Errorf("services[%q]: auth.username %q must not contain \":\" (RFC 7617 §2 — the Basic credential is built as \"username:secret\", so a colon in the username changes what the upstream parses as each half)", name, sc.Auth.Username)
		}
		if sc.Auth.SecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind basic requires \"secret_key\"", name)
		}
	case apigateway.AuthHeader:
		if sc.Auth.Header == "" {
			return fmt.Errorf("services[%q]: auth.kind header requires \"header\" (the header name to set)", name)
		}
		// isValidHTTPHeaderFieldName (RFC 7230 §3.2's "token" grammar) —
		// codex review finding: without this, a typo'd or otherwise-invalid
		// header name (whitespace, a stray ":", a control character, ...)
		// passed config-load validation cleanly and only surfaced as a
		// request-time failure — worth catching at `boid start`/`config
		// apply` time instead, the same "fail loud with the offending
		// name" posture every other leaf here gets.
		if !isValidHTTPHeaderFieldName(sc.Auth.Header) {
			return fmt.Errorf("services[%q]: auth.header %q is not a valid HTTP header field name", name, sc.Auth.Header)
		}
		// codex review finding (round 5): "Host" is a syntactically valid
		// header field name (passes the RFC 7230 token check above) but Go's
		// net/http treats it specially for an OUTGOING request — the
		// Host header actually sent on the wire comes from Request.Host (or
		// Request.URL.Host if that's empty), never from Header["Host"].
		// apigateway.CredentialProvider.Inject's `req.Header.Set(rs.auth.Header,
		// secret)` for auth.kind: header would therefore silently do nothing
		// useful: Inject reports success, Server.ServeHTTP proceeds believing
		// the request is authenticated, and the secret never reaches the
		// upstream on any channel — exactly the "gateway forwards an
		// unauthenticated request while believing it succeeded" failure the
		// whole gateway (failFastTransport's own doc comment: "this request
		// is authenticated, or it does not go out") exists to prevent. The
		// other names here are RFC 7230 §6.1's hop-by-hop set plus
		// Content-Length: net/http's Transport computes/manages all of them
		// itself for an outgoing request, so setting any of them via Header
		// is unreliable at best (silently overridden) and protocol-breaking
		// at worst (a body whose real length doesn't match a forged
		// Content-Length) — none of them is a legitimate place to carry a
		// custom credential regardless.
		if reservedHeaderNames[http.CanonicalHeaderKey(sc.Auth.Header)] {
			return fmt.Errorf("services[%q]: auth.header %q is a reserved/transport header that net/http manages itself for an outgoing request — it cannot carry a credential (the secret would silently never reach the upstream)", name, sc.Auth.Header)
		}
		if sc.Auth.SecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind header requires \"secret_key\"", name)
		}
	case apigateway.AuthQuery:
		if sc.Auth.Query == "" {
			return fmt.Errorf("services[%q]: auth.kind query requires \"query\" (the query parameter name to set)", name)
		}
		if sc.Auth.SecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind query requires \"secret_key\"", name)
		}
	case apigateway.AuthOAuth2:
		if sc.Auth.Provider == "" {
			return fmt.Errorf("services[%q]: auth.kind oauth2 requires \"provider\" (reserved for PR2 — docs/plans/api-gateway.md §論点4)", name)
		}
	}
	return nil
}

// APIGatewayServices resolves c.Services into the flat
// []apigateway.ServiceConfig list apigateway.NewCredentialProvider consumes
// (internal/server/wire.go), sorted by name for determinism. Mirrors
// GatewayConfig.HostConfigs's own shape and its "already validated by
// Config.UnmarshalYAML" invariant — an entry that fails validateServiceConfig
// here (a hand-built Config that skipped validation) is skipped silently
// rather than surfaced as an error, since this method has no error return
// and never should have one under that invariant.
func (c Config) APIGatewayServices() []apigateway.ServiceConfig {
	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]apigateway.ServiceConfig, 0, len(names))
	for _, name := range names {
		sc := c.Services[name]
		if err := validateServiceConfig(name, sc); err != nil {
			continue
		}
		out = append(out, apigateway.ServiceConfig{
			Name:    name,
			BaseURL: sc.BaseURL,
			Auth: apigateway.ServiceAuth{
				Kind:      apigateway.AuthKind(sc.Auth.Kind),
				SecretKey: sc.Auth.SecretKey,
				Username:  sc.Auth.Username,
				Header:    sc.Auth.Header,
				Query:     sc.Auth.Query,
				Provider:  sc.Auth.Provider,
			},
		})
	}
	return out
}

// isValidHTTPHeaderFieldName reports whether name is a valid HTTP header
// field name — RFC 7230 §3.2/§3.2.6's "token" grammar: one or more
// characters from a fixed set (letters, digits, and
// "!#$%&'*+-.^_`|~"), nothing else (no whitespace, no ":", no control
// characters). Hand-rolled rather than importing
// golang.org/x/net/http/httpguts.ValidHeaderFieldName (which does exactly
// this): that package transitively pulls in golang.org/x/text (a dependency
// this module's go.sum does not otherwise need) for a check simple enough
// to reimplement directly, matching CLAUDE.md's "外部ライブラリは最小限"
// rule.
func isValidHTTPHeaderFieldName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '!' || r == '#' || r == '$' || r == '%' || r == '&' || r == '\'' ||
			r == '*' || r == '+' || r == '-' || r == '.' || r == '^' || r == '_' ||
			r == '`' || r == '|' || r == '~':
		default:
			return false
		}
	}
	return true
}

// reservedHeaderNames is the set of header field names (canonicalized via
// http.CanonicalHeaderKey, so lookups are case-insensitive) that
// auth.kind: header must not use (codex review round 5 finding): net/http's
// Transport computes or manages every one of these itself for an outgoing
// request, so setting it via Header.Set is either silently ineffective
// ("Host" — Go never reads Header["Host"] when writing the request line; it
// always uses Request.Host/Request.URL.Host instead, so
// CredentialProvider.Inject's req.Header.Set("Host", secret) would report
// success while the secret reaches the upstream on no channel at all — the
// exact "gateway forwards an unauthenticated request while believing it
// succeeded" failure this whole package exists to prevent) or
// protocol-breaking (the RFC 7230 §6.1 hop-by-hop set, plus
// Content-Length — a forged length that doesn't match the real body would
// produce a malformed request, and Transfer-Encoding/Connection/etc. are
// entirely re-derived by the transport regardless of what a handler sets).
// None of these is ever a legitimate place to carry a custom credential.
var reservedHeaderNames = map[string]bool{
	http.CanonicalHeaderKey("Host"):                true,
	http.CanonicalHeaderKey("Content-Length"):      true,
	http.CanonicalHeaderKey("Connection"):          true,
	http.CanonicalHeaderKey("Proxy-Connection"):    true,
	http.CanonicalHeaderKey("Keep-Alive"):          true,
	http.CanonicalHeaderKey("Proxy-Authenticate"):  true,
	http.CanonicalHeaderKey("Proxy-Authorization"): true,
	http.CanonicalHeaderKey("TE"):                  true,
	http.CanonicalHeaderKey("Trailer"):             true,
	http.CanonicalHeaderKey("Transfer-Encoding"):   true,
	http.CanonicalHeaderKey("Upgrade"):             true,
}
