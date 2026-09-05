package config

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/apigateway"
)

// ServiceAuthConfig configures how the API gateway authenticates requests
// to one service. Which fields are required depends on Kind — see
// validateServiceConfig.
type ServiceAuthConfig struct {
	// Kind selects the injection strategy: "bearer" / "basic" / "header" /
	// "query" / "oauth2".
	Kind string `yaml:"kind"`
	// SecretKey is a reference into the secret store
	// (internal/dispatcher/secret_store.go); never a plaintext value.
	// Required for bearer/basic/header/query; unused for oauth2.
	SecretKey string `yaml:"secret_key,omitempty"`
	// Username is the Basic-auth username. Required (and only meaningful)
	// for kind: basic — mutually exclusive with UsernameSecretKey (exactly
	// one of the two is required for kind: basic; see validateServiceConfig).
	Username string `yaml:"username,omitempty"`
	// UsernameSecretKey, when set (kind: basic only), resolves the
	// Basic-auth username from the secret store instead of using the
	// literal Username above — account-qualified the same way SecretKey
	// already is.
	//
	// NOT the same config path as ServiceConfig.UsernameSecretKey
	// (services.<name>.username_secret_key, uses:-entries only). This field
	// only applies to a free-form entry (a uses: entry's Auth block must be
	// the exact zero value, validateServiceConfig).
	UsernameSecretKey string `yaml:"username_secret_key,omitempty"`
	// Header is the header name to set. Required (and only meaningful) for
	// kind: header.
	Header string `yaml:"header,omitempty"`
	// Query is the query parameter name to set. Required (and only
	// meaningful) for kind: query.
	Query string `yaml:"query,omitempty"`
	// Provider names the OAuth2 provider (config.yaml oauth_providers:
	// block). Required (and only meaningful) for kind: oauth2.
	Provider string `yaml:"provider,omitempty"`
}

// ServiceConfig declares one logical service the API gateway can proxy job
// requests to.
type ServiceConfig struct {
	// BaseURL is the upstream base URL (scheme + host, optionally a path
	// prefix) — never exposed to the sandbox; only the logical service name
	// in the route path is.
	BaseURL string            `yaml:"base_url,omitempty"`
	Auth    ServiceAuthConfig `yaml:"auth"`
	// BaseURLSecretKey, when set, resolves BaseURL from the secret store
	// instead of using the literal field above — account-qualified the
	// same way auth.secret_key already is. Mutually exclusive with
	// BaseURL; exactly one is required for a free-form (non-uses:) entry.
	BaseURLSecretKey string `yaml:"base_url_secret_key,omitempty"`
	// AllowInsecure must be explicitly set to true for BaseURL to use any
	// scheme other than "https" — a fail-closed requirement so choosing a
	// plaintext upstream is always a conscious, config-visible decision.
	AllowInsecure bool `yaml:"allow_insecure,omitempty"`
	// AllowReadOnlyWrite opts this service out of the read-only→GET/HEAD-only
	// gate (apigateway.Server.ServeHTTP) so a read-only job token can still
	// POST/PUT/PATCH/DELETE to it. Deliberately config.yaml-only (never a
	// project.yaml/task_behaviors field): granting it from inside the repo
	// would let a prompt-injected agent grant itself write access to any
	// service already reachable from the job token. Defaults to false.
	AllowReadOnlyWrite bool `yaml:"allow_readonly_write,omitempty"`

	// RequireAccount, when true, rejects (400) any request to this service
	// that omits the credential-account qualifier (the request path's
	// service segment must be "<name>@<account>", not "<name>" alone).
	// Defaults to false, so an existing services.<name> entry's behavior is
	// unchanged until an operator opts in explicitly.
	//
	// Deliberately config.yaml-only (never a project.yaml/task_behaviors
	// field) for the same reason AllowReadOnlyWrite above is: this is a
	// gateway-level credential-selection constraint the daemon operator
	// controls, not something a job running inside the repo should be able
	// to toggle either way.
	RequireAccount bool `yaml:"require_account,omitempty"`

	// Uses references an installed Integration Pack's service profile
	// instead of hand-writing BaseURL/Auth. Format:
	// "<pack名>/<profile名>@<pack version>" — parsed by ParseUsesReference.
	// Mutually exclusive with BaseURL/Auth (validateServiceConfig). This
	// package only checks the reference's SYNTAX; resolving it against the
	// installed Pack registry is internal/integrationpack's job — it has
	// the loaded Packs this package deliberately does not depend on.
	Uses string `yaml:"uses,omitempty"`
	// Endpoint fills in the resolved service profile's endpoint.configurable
	// slot. Only meaningful — and only accepted by validateServiceConfig —
	// when Uses is set; the profile itself decides whether a value is
	// required, forbidden, or unused
	// (internal/integrationpack.DesugarService enforces the pairing).
	Endpoint string `yaml:"endpoint,omitempty"`
	// EndpointSecretKey, when set, resolves the profile's
	// endpoint.configurable slot from the secret store instead of using
	// the literal Endpoint above. Mutually exclusive with Endpoint. Only
	// meaningful — and only accepted by validateServiceConfig — when Uses
	// is set, mirroring Endpoint's own scoping.
	EndpointSecretKey string `yaml:"endpoint_secret_key,omitempty"`
	// Credentials binds each of the resolved service profile's declared
	// credential slot names to a SecretStore key reference — never a
	// plaintext value, the same convention ServiceAuthConfig.SecretKey
	// already has. Only meaningful — and only accepted by
	// validateServiceConfig — when Uses is set.
	Credentials map[string]string `yaml:"credentials,omitempty"`
	// Username fills in the resolved service profile's INSTANCE-SPECIFIC
	// Basic-auth username slot (internal/integrationpack.CredentialSlot.
	// UsernameFrom == UsernameFromInstance) — e.g. a login email that
	// differs per instance rather than a Pack-fixed constant. Deliberately
	// a TOP-LEVEL, PLAINTEXT field, not folded into Credentials above:
	// Credentials only ever binds SecretStore key references, and an email
	// address is config, not a secret. Only meaningful — and only accepted
	// by validateServiceConfig — when Uses is set; whether the resolved
	// profile's slot actually accepts it is
	// internal/integrationpack.DesugarService's job.
	Username string `yaml:"username,omitempty"`
	// UsernameSecretKey, when set, resolves the profile's INSTANCE-SPECIFIC
	// Basic-auth username slot from the secret store instead of using the
	// literal Username above. Mutually exclusive with Username. Only
	// meaningful — and only accepted by validateServiceConfig — when Uses
	// is set, mirroring Username's own scoping.
	//
	// NOT the same config path as ServiceAuthConfig.UsernameSecretKey
	// (services.<name>.auth.username_secret_key, free-form entries only).
	UsernameSecretKey string `yaml:"username_secret_key,omitempty"`
}

// ParseUsesReference parses a services.<name>.uses value of the form
// "<pack名>/<profile名>@<pack version>" into its three components. Exported
// so internal/integrationpack — which resolves the reference against the
// loaded Pack registry — parses it with the exact same rule this package's
// own load-time syntax check (validateServiceConfig) uses.
func ParseUsesReference(uses string) (pack, profile, version string, err error) {
	malformed := fmt.Errorf("%q must have the form \"<pack>/<profile>@<version>\"", uses)
	at := strings.LastIndex(uses, "@")
	if at <= 0 || at == len(uses)-1 {
		return "", "", "", malformed
	}
	ref, version := uses[:at], uses[at+1:]
	slash := strings.Index(ref, "/")
	if slash <= 0 || slash == len(ref)-1 {
		return "", "", "", malformed
	}
	return ref[:slash], ref[slash+1:], version, nil
}

// validServiceAuthKinds is the accepted auth.kind set. oauth2 validates only
// that Provider is set.
var validServiceAuthKinds = map[string]bool{
	string(apigateway.AuthBearer): true,
	string(apigateway.AuthBasic):  true,
	string(apigateway.AuthHeader): true,
	string(apigateway.AuthQuery):  true,
	string(apigateway.AuthOAuth2): true,
}

// ValidateServiceURL validates rawURL for the safety properties every
// service's outbound upstream address must have:
//   - an absolute URL with an explicit scheme and host
//   - no query string or fragment (apigateway.Server always forwards the
//     inbound request's own RawQuery verbatim)
//   - https, unless allowInsecure is true
//   - only http/https schemes even with allowInsecure set (the gateway's
//     outbound Transport only ever speaks http/https)
//
// Shared by validateServiceConfig's base_url check and its uses:-branch
// endpoint check. serviceName/fieldName only name the offending
// services.<name> entry/key in the returned error — fieldName is
// "base_url" or "endpoint" depending on the caller.
//
// A thin wrapper around apigateway.ValidateBaseURL: the same check needs to
// run at request time too (for a base_url_secret_key-backed service), and
// internal/apigateway cannot import this package back, so the
// implementation lives there and this function delegates.
func ValidateServiceURL(serviceName, fieldName, rawURL string, allowInsecure bool) error {
	return apigateway.ValidateBaseURL(serviceName, fieldName, rawURL, allowInsecure)
}

// validateServiceConfig validates one services.<name> entry, returning a
// descriptive error naming both the service and the missing/invalid field.
func validateServiceConfig(name string, sc ServiceConfig) error {
	// The service registry built from this config is keyed verbatim by the
	// services.<name> map key, but ResolveEnabledServices trims every
	// service name it resolves before matching — so a whitespace-padded
	// name would validate cleanly here yet never resolve for any workspace.
	if trimmed := strings.TrimSpace(name); trimmed != name {
		return fmt.Errorf("services[%q]: service name must not have leading/trailing whitespace (did you mean %q?)", name, trimmed)
	}
	if name == "" {
		return fmt.Errorf("services: a service name must not be empty")
	}
	// "@" in the request path's service segment separates the service name
	// from an optional credential-account qualifier ("freee@ubs" —
	// apigateway.parsePath), so a services.<name> key containing "@" would
	// be permanently unreachable and ambiguous with that construct.
	if strings.Contains(name, "@") {
		return fmt.Errorf("services[%q]: service name must not contain \"@\" — the gateway path reserves \"@\" to separate a service name from an optional credential-account qualifier (docs/plans/api-gateway-credential-accounts.md)", name)
	}

	// uses: a Pack-profile-backed instance is a completely different shape
	// from a free-form base_url/auth entry — its base_url/auth come from
	// the resolved profile instead — so the two are mutually exclusive,
	// checked before the base_url/auth requirements below ever run. This
	// package can only validate uses:'s own syntax (ParseUsesReference);
	// resolving it against the installed Pack registry is
	// internal/integrationpack's job.
	if sc.Uses != "" {
		if sc.BaseURL != "" {
			return fmt.Errorf("services[%q]: \"uses\" and \"base_url\" are mutually exclusive — base_url comes from the resolved Integration Pack service profile instead", name)
		}
		if sc.BaseURLSecretKey != "" {
			return fmt.Errorf("services[%q]: \"uses\" and \"base_url_secret_key\" are mutually exclusive — base_url comes from the resolved Integration Pack service profile instead", name)
		}
		if sc.Auth != (ServiceAuthConfig{}) {
			return fmt.Errorf("services[%q]: \"uses\" and \"auth\" are mutually exclusive — credential injection comes from the resolved Integration Pack service profile instead (set \"credentials\" to bind SecretStore keys to the profile's declared slots)", name)
		}
		if _, _, _, err := ParseUsesReference(sc.Uses); err != nil {
			return fmt.Errorf("services[%q]: \"uses\": %w", name, err)
		}
		// A uses:-based instance's resolved BaseURL comes from Endpoint
		// (internal/integrationpack.DesugarService), so it must satisfy the
		// same safety properties base_url does. Whether Endpoint is
		// actually required depends on the resolved service profile
		// (unknown here), so an empty Endpoint is not rejected by this
		// package; DesugarService enforces that half once the Pack
		// registry is known.
		if sc.Endpoint != "" {
			if err := ValidateServiceURL(name, "endpoint", sc.Endpoint, sc.AllowInsecure); err != nil {
				return err
			}
		}
		// endpoint / endpoint_secret_key: mutually exclusive. Not validated
		// as a URL here — that value doesn't exist at config-load time;
		// apigateway.CredentialProvider.BaseURLFor resolves and validates
		// it at request time instead.
		if sc.Endpoint != "" && sc.EndpointSecretKey != "" {
			return fmt.Errorf("services[%q]: \"endpoint\" and \"endpoint_secret_key\" are mutually exclusive", name)
		}
		// username / username_secret_key: mutually exclusive. Whether
		// either is actually required depends on the resolved profile's
		// credential slot (unknown here), so neither is required by this
		// package; internal/integrationpack.DesugarService enforces that
		// half once the Pack registry is known.
		if sc.Username != "" && sc.UsernameSecretKey != "" {
			return fmt.Errorf("services[%q]: \"username\" and \"username_secret_key\" are mutually exclusive", name)
		}
		return nil
	}
	// endpoint:/credentials:/username: only mean anything alongside uses:
	// (they fill in / bind a resolved Pack service profile's declared slots)
	// — rejecting them outright on a free-form entry catches a likely
	// stray copy-paste at config-load time instead of the value silently
	// doing nothing.
	if sc.Endpoint != "" {
		return fmt.Errorf("services[%q]: \"endpoint\" requires \"uses\" to be set", name)
	}
	if sc.EndpointSecretKey != "" {
		return fmt.Errorf("services[%q]: \"endpoint_secret_key\" requires \"uses\" to be set", name)
	}
	if len(sc.Credentials) > 0 {
		return fmt.Errorf("services[%q]: \"credentials\" requires \"uses\" to be set", name)
	}
	if sc.Username != "" {
		return fmt.Errorf("services[%q]: \"username\" requires \"uses\" to be set (for a free-form entry, use \"auth.username\" instead)", name)
	}
	if sc.UsernameSecretKey != "" {
		return fmt.Errorf("services[%q]: \"username_secret_key\" requires \"uses\" to be set (for a free-form entry, use \"auth.username_secret_key\" instead)", name)
	}

	// base_url / base_url_secret_key: mutually exclusive, exactly one
	// required for a free-form entry.
	if sc.BaseURL == "" && sc.BaseURLSecretKey == "" {
		return fmt.Errorf("services[%q]: missing required \"base_url\" (or \"base_url_secret_key\") field", name)
	}
	if sc.BaseURL != "" && sc.BaseURLSecretKey != "" {
		return fmt.Errorf("services[%q]: \"base_url\" and \"base_url_secret_key\" are mutually exclusive", name)
	}
	if sc.BaseURL != "" {
		// Parsed and scheme/host-checked here, at config-load time, rather
		// than left for apigateway.NewCredentialProvider's own defensive
		// parse to discover later — a malformed base_url should fail
		// `boid start` loudly, not silently vanish from the gateway's
		// service registry with only a log warning once the daemon is
		// already running.
		//
		// This invariant does NOT extend to base_url_secret_key: that
		// value doesn't exist at config-load time — it lives in the secret
		// store, resolved and validated by
		// apigateway.CredentialProvider.BaseURLFor at request time instead.
		if err := ValidateServiceURL(name, "base_url", sc.BaseURL, sc.AllowInsecure); err != nil {
			return err
		}
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
		// auth.username / auth.username_secret_key: mutually exclusive,
		// exactly one required.
		if sc.Auth.Username == "" && sc.Auth.UsernameSecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind basic requires \"username\" (or \"username_secret_key\")", name)
		}
		if sc.Auth.Username != "" && sc.Auth.UsernameSecretKey != "" {
			return fmt.Errorf("services[%q]: auth.username and auth.username_secret_key are mutually exclusive", name)
		}
		// RFC 7617 §2: the userid (username) component of a Basic
		// credential must not contain a colon — the credential is built as
		// "userid:password" and the first colon is what separates the two,
		// so a username containing one changes what the upstream actually
		// parses as the username vs. the password. Only checked for the
		// literal Username here — a UsernameSecretKey-backed value doesn't
		// exist at config-load time; CredentialProvider.resolveUsername
		// re-applies this exact check once the secret store resolves it.
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
		if !isValidHTTPHeaderFieldName(sc.Auth.Header) {
			return fmt.Errorf("services[%q]: auth.header %q is not a valid HTTP header field name", name, sc.Auth.Header)
		}
		// "Host" is a syntactically valid header field name (passes the
		// RFC 7230 token check above) but Go's net/http treats it
		// specially for an outgoing request — the Host header actually
		// sent on the wire comes from Request.Host, never from
		// Header["Host"], so CredentialProvider.Inject's Header.Set for
		// auth.kind: header would silently do nothing useful. The other
		// names here are RFC 7230 §6.1's hop-by-hop set plus
		// Content-Length, which net/http's Transport computes/manages
		// itself for an outgoing request.
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
			return fmt.Errorf("services[%q]: auth.kind oauth2 requires \"provider\" (docs/plans/api-gateway.md §論点4 — names an oauth_providers.<name> entry)", name)
		}
		// Provider is deliberately NOT cross-validated against
		// oauth_providers here — see
		// TestLoadFromPath_Services_OAuth2ProviderReferenceNotCrossValidated
		// (oauth_providers_test.go): this mirrors every other secret-store-
		// shaped reference in this file, none of which is validated
		// against its actual target at config-load time either.
	}
	return nil
}

// APIGatewayServices resolves c.Services into the flat
// []apigateway.ServiceConfig list apigateway.NewCredentialProvider consumes,
// sorted by name for determinism. An entry that fails validateServiceConfig
// here (a hand-built Config that skipped validation) is skipped silently
// rather than surfaced as an error, since this method has no error return.
//
// A uses: entry is deliberately excluded from the output — it has no
// BaseURL/Auth of its own to convert (those come from the resolved
// Integration Pack service profile, which this package cannot reach).
// internal/integrationpack.ResolveServices is what combines this method's
// output with its own Pack-resolved uses: entries into the full flat list.
func (c Config) APIGatewayServices() []apigateway.ServiceConfig {
	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]apigateway.ServiceConfig, 0, len(names))
	for _, name := range names {
		sc := c.Services[name]
		if sc.Uses != "" {
			continue
		}
		if err := validateServiceConfig(name, sc); err != nil {
			continue
		}
		out = append(out, apigateway.ServiceConfig{
			Name:             name,
			BaseURL:          sc.BaseURL,
			BaseURLSecretKey: sc.BaseURLSecretKey,
			AllowInsecure:    sc.AllowInsecure,
			Auth: apigateway.ServiceAuth{
				Kind:              apigateway.AuthKind(sc.Auth.Kind),
				SecretKey:         sc.Auth.SecretKey,
				Username:          sc.Auth.Username,
				UsernameSecretKey: sc.Auth.UsernameSecretKey,
				Header:            sc.Auth.Header,
				Query:             sc.Auth.Query,
				Provider:          sc.Auth.Provider,
			},
			AllowReadOnlyWrite: sc.AllowReadOnlyWrite,
			RequireAccount:     sc.RequireAccount,
		})
	}
	return out
}

// UsesServices returns the subset of c.Services with uses: set, keyed by
// instance name — the counterpart to APIGatewayServices() (which
// deliberately excludes them) that internal/integrationpack.ResolveServices
// consumes to know which instances to desugar against the loaded Pack
// registry.
func (c Config) UsesServices() map[string]ServiceConfig {
	out := make(map[string]ServiceConfig)
	for name, sc := range c.Services {
		if sc.Uses != "" {
			out[name] = sc
		}
	}
	return out
}

// OAuthProviderConfig declares one config.yaml oauth_providers.<name> entry
// — the OAuth2 token-endpoint identity a services.*.auth.kind: oauth2
// entry's `provider` field references.
type OAuthProviderConfig struct {
	// TokenEndpoint is the provider's OAuth2 token endpoint (RFC 6749
	// §3.2), e.g. https://accounts.secure.freee.co.jp/public_api/token.
	// Always https.
	TokenEndpoint string `yaml:"token_endpoint"`
	// ClientID is the OAuth2 client_id. Not a secret by RFC 6749 §2.2's own
	// classification (a public identifier), so — unlike ClientSecretKey —
	// it is written directly here rather than referenced through the
	// secret store.
	ClientID string `yaml:"client_id"`
	// ClientSecretKey is a secret-store key reference (never a plaintext
	// value) for a confidential client's client_secret. Empty for a public
	// client (PKCE, no client_secret) — apigateway.OAuth2TokenSource simply
	// omits client_secret from the token request when this is empty.
	ClientSecretKey string `yaml:"client_secret_key,omitempty"`
	// Scopes is consumed by `boid secret oauth login`'s authorization-URL
	// construction (loopback/manual) and device-authorization request
	// (device). Never sent on an authorization_code refresh_token grant —
	// see apigateway.OAuth2TokenSource.callRefreshTokenEndpoint. A
	// client_credentials grant DOES send Scopes on every token request —
	// see callClientCredentialsTokenEndpoint instead.
	Scopes []string `yaml:"scopes,omitempty"`

	// Flow selects which of the three initial-grant flows `boid secret
	// oauth login <service>` uses for this provider: "device" / "loopback"
	// / "manual". Empty ("") means this provider has no login flow
	// configured — an operator can still seed refresh_token by hand via
	// `boid secret set`, but `boid secret oauth login` for it fails with a
	// clear "no flow configured" error rather than guessing.
	Flow string `yaml:"flow,omitempty"`
	// AuthorizationEndpoint is the provider's OAuth2 authorization endpoint
	// (RFC 6749 §3.1) — required (and only meaningful) when Flow is
	// "loopback" or "manual".
	AuthorizationEndpoint string `yaml:"authorization_endpoint,omitempty"`
	// DeviceAuthorizationEndpoint is the provider's RFC 8628 §3.1 device
	// authorization endpoint — required (and only meaningful) when Flow is
	// "device".
	DeviceAuthorizationEndpoint string `yaml:"device_authorization_endpoint,omitempty"`
	// AuthorizeParams is a fixed set of extra parameters appended verbatim
	// to the authorization request (loopback/manual: query parameters;
	// device: form fields on the device authorization request) — see
	// apigateway.OAuthProviderConfig.AuthorizeParams for the motivating
	// example (Google's access_type/prompt). Keys colliding with a
	// protocol-reserved parameter are rejected below.
	AuthorizeParams map[string]string `yaml:"authorize_params,omitempty"`

	// Grant selects which RFC 6749 grant apigateway.OAuth2TokenSource.
	// refresh performs for this provider: "authorization_code" (the
	// default) or "client_credentials" (RFC 6749 §4.4, 2-legged / app-only,
	// Service Principal). Deliberately a separate field from Flow, not a
	// fourth Flow value — see apigateway.OAuthProviderConfig.Grant. Empty
	// ("") means "authorization_code".
	Grant string `yaml:"grant,omitempty"`
}

// reservedAuthorizeParamNames is every query/form parameter this package's
// own login-flow construction (apigateway/login.go's buildAuthorizeURL /
// postDeviceAuthorizationRequest) sets itself — an operator-supplied
// authorize_params entry using one of these keys would collide
// unpredictably with the protocol machinery, so config-load rejects it
// outright instead.
var reservedAuthorizeParamNames = map[string]bool{
	"response_type":         true,
	"client_id":             true,
	"client_secret":         true,
	"redirect_uri":          true,
	"scope":                 true,
	"state":                 true,
	"code_challenge":        true,
	"code_challenge_method": true,
	"code":                  true,
	"code_verifier":         true,
	"grant_type":            true,
	"device_code":           true,
}

// validLoginFlows mirrors apigateway.ValidLoginFlows exactly (Flow is a
// plain string here, an apigateway.LoginFlow there).
var validLoginFlows = map[string]bool{
	string(apigateway.LoginFlowDevice):   true,
	string(apigateway.LoginFlowLoopback): true,
	string(apigateway.LoginFlowManual):   true,
}

// validOAuthGrants mirrors apigateway.ValidOAuthGrants exactly (Grant is a
// plain string here, an apigateway.OAuthGrant there).
var validOAuthGrants = map[string]bool{
	string(apigateway.GrantAuthorizationCode): true,
	string(apigateway.GrantClientCredentials): true,
}

// validateOAuthProviderConfig validates one oauth_providers.<name> entry.
func validateOAuthProviderConfig(name string, pc OAuthProviderConfig) error {
	if trimmed := strings.TrimSpace(name); trimmed != name {
		return fmt.Errorf("oauth_providers[%q]: provider name must not have leading/trailing whitespace (did you mean %q?)", name, trimmed)
	}
	if name == "" {
		return fmt.Errorf("oauth_providers: a provider name must not be empty")
	}
	// The account-qualified oauth2 credential key is
	// "oauth2:<provider>@<account>:...", so a provider name containing "@"
	// would collide with that separator.
	if strings.Contains(name, "@") {
		return fmt.Errorf("oauth_providers[%q]: provider name must not contain \"@\" — the account-qualified credential key reserves \"@\" to separate a provider name from an account qualifier (docs/plans/api-gateway-credential-accounts.md)", name)
	}
	if pc.TokenEndpoint == "" {
		return fmt.Errorf("oauth_providers[%q]: missing required \"token_endpoint\" field", name)
	}
	u, err := url.Parse(pc.TokenEndpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("oauth_providers[%q]: \"token_endpoint\" must be an absolute URL with a scheme and host (got %q)", name, pc.TokenEndpoint)
	}
	// Unlike services.*.base_url, there is deliberately no allow_insecure
	// escape hatch here: token_endpoint is always a daemon-to-real-OAuth2-
	// provider call, never a sandbox-facing or internal-test-API endpoint.
	if u.Scheme != "https" {
		return fmt.Errorf("oauth_providers[%q]: \"token_endpoint\" scheme must be https (got %q) — an OAuth2 token endpoint always requires TLS", name, u.Scheme)
	}
	if pc.ClientID == "" {
		return fmt.Errorf("oauth_providers[%q]: missing required \"client_id\" field", name)
	}

	if pc.Grant != "" && !validOAuthGrants[pc.Grant] {
		return fmt.Errorf("oauth_providers[%q]: \"grant\": unrecognized %q (want one of authorization_code, client_credentials)", name, pc.Grant)
	}
	if apigateway.OAuthGrant(pc.Grant) == apigateway.GrantClientCredentials {
		// RFC 6749 §4.4.2: client_credentials requires a confidential
		// client. This catches an unset client_secret_key; a
		// client_secret_key that IS set but resolves to an empty
		// secret-store value cannot be caught here (config load has no
		// secret-store access) — apigateway.OAuth2TokenSource.
		// refreshClientCredentials catches that half at refresh time
		// instead.
		if pc.ClientSecretKey == "" {
			return fmt.Errorf("oauth_providers[%q]: \"client_secret_key\" is required when \"grant\" is client_credentials (RFC 6749 §4.4.2 requires a confidential client)", name)
		}
		if pc.Flow != "" {
			return fmt.Errorf("oauth_providers[%q]: \"flow\" must not be set when \"grant\" is client_credentials — client_credentials (RFC 6749 §4.4) has no login flow at all", name)
		}
	}

	if pc.Flow != "" {
		if !validLoginFlows[pc.Flow] {
			return fmt.Errorf("oauth_providers[%q]: \"flow\": unrecognized %q (want one of device, loopback, manual)", name, pc.Flow)
		}
		switch apigateway.LoginFlow(pc.Flow) {
		case apigateway.LoginFlowDevice:
			if err := validateOAuthEndpointURL(name, "device_authorization_endpoint", pc.DeviceAuthorizationEndpoint); err != nil {
				return err
			}
		case apigateway.LoginFlowLoopback, apigateway.LoginFlowManual:
			if err := validateOAuthEndpointURL(name, "authorization_endpoint", pc.AuthorizationEndpoint); err != nil {
				return err
			}
		}
	}
	for k := range pc.AuthorizeParams {
		if reservedAuthorizeParamNames[k] {
			return fmt.Errorf("oauth_providers[%q]: \"authorize_params\" must not set %q — it is a protocol-reserved parameter this package's login flow already sets itself", name, k)
		}
	}
	return nil
}

// validateOAuthEndpointURL validates one flow-conditional endpoint field
// (authorization_endpoint or device_authorization_endpoint), required and
// https-only for the same reason token_endpoint above is: both are always a
// daemon-to-real-OAuth2-provider call.
func validateOAuthEndpointURL(providerName, field, value string) error {
	if value == "" {
		return fmt.Errorf("oauth_providers[%q]: \"flow\" requires %q to be set", providerName, field)
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("oauth_providers[%q]: %q must be an absolute URL with a scheme and host (got %q)", providerName, field, value)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("oauth_providers[%q]: %q scheme must be https (got %q)", providerName, field, u.Scheme)
	}
	return nil
}

// APIGatewayOAuthProviders resolves c.OAuthProviders into the flat
// []apigateway.OAuthProviderConfig list apigateway.NewOAuth2TokenSource
// consumes, sorted by name for determinism. An entry that fails
// validateOAuthProviderConfig here (a hand-built Config that skipped
// validation) is skipped silently rather than surfaced as an error, since
// this method has no error return.
func (c Config) APIGatewayOAuthProviders() []apigateway.OAuthProviderConfig {
	names := make([]string, 0, len(c.OAuthProviders))
	for name := range c.OAuthProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]apigateway.OAuthProviderConfig, 0, len(names))
	for _, name := range names {
		pc := c.OAuthProviders[name]
		if err := validateOAuthProviderConfig(name, pc); err != nil {
			continue
		}
		var authorizeParams map[string]string
		if len(pc.AuthorizeParams) > 0 {
			authorizeParams = make(map[string]string, len(pc.AuthorizeParams))
			for k, v := range pc.AuthorizeParams {
				authorizeParams[k] = v
			}
		}
		out = append(out, apigateway.OAuthProviderConfig{
			Name:                        name,
			TokenEndpoint:               pc.TokenEndpoint,
			ClientID:                    pc.ClientID,
			ClientSecretKey:             pc.ClientSecretKey,
			Scopes:                      append([]string(nil), pc.Scopes...),
			Flow:                        apigateway.LoginFlow(pc.Flow),
			AuthorizationEndpoint:       pc.AuthorizationEndpoint,
			DeviceAuthorizationEndpoint: pc.DeviceAuthorizationEndpoint,
			AuthorizeParams:             authorizeParams,
			Grant:                       apigateway.OAuthGrant(pc.Grant),
		})
	}
	return out
}

// isValidHTTPHeaderFieldName reports whether name is a valid HTTP header
// field name — RFC 7230 §3.2/§3.2.6's "token" grammar: one or more
// characters from a fixed set (letters, digits, and
// "!#$%&'*+-.^_`|~"), nothing else. Hand-rolled rather than importing
// golang.org/x/net/http/httpguts.ValidHeaderFieldName: that package
// transitively pulls in golang.org/x/text for a check simple enough to
// reimplement directly, matching CLAUDE.md's "外部ライブラリは最小限" rule.
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
// auth.kind: header must not use: net/http's Transport computes or manages
// every one of these itself for an outgoing request, so setting it via
// Header.Set is either silently ineffective ("Host") or protocol-breaking
// (the RFC 7230 §6.1 hop-by-hop set, plus Content-Length). None of these is
// ever a legitimate place to carry a custom credential.
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
