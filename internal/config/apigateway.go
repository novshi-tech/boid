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
	// for kind: basic — mutually exclusive with UsernameSecretKey (exactly
	// one of the two is required for kind: basic; see validateServiceConfig).
	Username string `yaml:"username,omitempty"`
	// UsernameSecretKey, when set (kind: basic only), resolves the
	// Basic-auth username from the secret store instead of using the
	// literal Username above — account-qualified the same way SecretKey
	// already is (docs/plans/api-gateway-credential-accounts.md D12).
	// Motivating case: a Jira-Cloud-shaped service where the login email
	// differs per tenant/account, not just the token. A secret-store value
	// is not a "secret" in the traditional sense here (an email address is
	// config, not confidential material) — this deliberately reuses the
	// existing account-qualified secret-key mechanism rather than growing
	// the config SHAPE (docs/plans/api-gateway-credential-accounts.md's own
	// "却下 3": a services.<name>.accounts.<x> nested block was rejected
	// for the OAuth case, and re-proposing an equivalent nested block here
	// for Basic-auth services was considered and rejected again for the
	// same reason — a growing schema costs more than the secret-store's
	// mild semantic mismatch for two non-secret leaf values).
	//
	// NOT the same config path as the top-level ServiceConfig.
	// UsernameSecretKey (services.<name>.username_secret_key, no "auth."
	// prefix, uses:-entries only — D13). This field only ever applies to a
	// free-form entry (a uses: entry's Auth block must be the exact zero
	// value, validateServiceConfig); the top-level field only ever applies
	// to a uses: entry (mirroring where the existing literal Username
	// fields already live on each shape). See that field's own doc comment
	// for why the two look similar but are scoped to mutually-exclusive
	// entry shapes.
	UsernameSecretKey string `yaml:"username_secret_key,omitempty"`
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
	BaseURL string            `yaml:"base_url,omitempty"`
	Auth    ServiceAuthConfig `yaml:"auth"`
	// BaseURLSecretKey, when set, resolves BaseURL from the secret store
	// instead of using the literal field above — account-qualified the
	// same way auth.secret_key already is (docs/plans/api-gateway-
	// credential-accounts.md D12). Mutually exclusive with BaseURL; exactly
	// one is required for a free-form (non-uses:) entry. Motivating case:
	// Jira Cloud, where the whole upstream tenant (subdomain) differs per
	// account, not just the credential — this is the same "extend the
	// existing account-qualified secret-key mechanism rather than grow the
	// config shape" trade-off UsernameSecretKey documents; see that field's
	// own doc comment for the full rationale (却下 3).
	BaseURLSecretKey string `yaml:"base_url_secret_key,omitempty"`
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
	// AllowReadOnlyWrite opts this service out of the read-only→GET/HEAD-only
	// gate (apigateway.Server.ServeHTTP / apigateway.ServiceConfig's own doc
	// comment) so a read-only job token can still POST/PUT/PATCH/DELETE to
	// it — e.g. a Slack "post completion report" webhook a review job (which
	// is readonly:true so it can't touch the sandbox workspace) should still
	// be able to reach. Deliberately config.yaml-only (never a project.yaml/
	// task_behaviors field): granting it from inside the repo would let a
	// prompt-injected agent grant itself write access to any service already
	// reachable from the job token, defeating the readonly gate entirely
	// (docs/plans/api-gateway.md:59-62's "project.yaml には credential
	// アクセス権限を置かない" decision). Defaults to false (fail-closed) —
	// an operator must explicitly opt each service in.
	AllowReadOnlyWrite bool `yaml:"allow_readonly_write,omitempty"`

	// RequireAccount, when true, rejects (400) any request to this service
	// that omits the credential-account qualifier (docs/plans/
	// api-gateway-credential-accounts.md D5 — the request path's service
	// segment must be "<name>@<account>", not "<name>" alone). It is the
	// safety valve the design doc's "freee の移行手順" leans on: once an
	// operator has moved every real caller over to an explicit account
	// (`freee@ubs` / `freee@nvt`), flipping this on makes a caller that
	// forgot the "@<account>" qualifier fail loudly (400) instead of
	// silently resolving whatever account-less secret happens to still be
	// sitting in the store — the exact "指定漏れが既定アカウントへ落ちる事故"
	// this field exists to close off. Defaults to false, so every existing
	// services.<name> entry's behavior is byte-identical until an operator
	// opts in explicitly (D5's own "既定 false なので既存 service の挙動は
	// 変わらない").
	//
	// Deliberately config.yaml-only (never a project.yaml/task_behaviors
	// field) — the same posture AllowReadOnlyWrite above documents, and for
	// the same reason: this is a gateway-level credential-selection
	// constraint the daemon operator controls, not something a job running
	// inside the repo should ever be able to toggle. Letting project.yaml
	// turn this OFF would let a prompt-injected agent silently defeat the
	// very safety net it exists to provide (an account-less request that
	// should fail loudly instead reaching whatever credential the
	// account-less key happens to resolve to); letting it turn this ON
	// would let a job unilaterally start rejecting its own account-less
	// requests, an odd but still daemon-operator-only decision about how
	// THIS service's credentials are meant to be selected.
	RequireAccount bool `yaml:"require_account,omitempty"`

	// Uses references an installed Integration Pack's service profile
	// instead of hand-writing BaseURL/Auth (docs/plans/signal-driven-review.md
	// §7.2, docs/plans/signal-ingest-detailed-design.md §6.1). Format:
	// "<pack名>/<profile名>@<pack version>" — parsed by ParseUsesReference.
	// Mutually exclusive with BaseURL/Auth (validateServiceConfig). This
	// package can only check the reference's SYNTAX; resolving it against
	// the installed Pack registry (pack existence, profile/version match,
	// credential slot binding, endpoint requirement) is
	// internal/integrationpack's job — it has the loaded Packs this package
	// deliberately does not depend on (Q16: core must not import
	// service-specific Pack internals).
	Uses string `yaml:"uses,omitempty"`
	// Endpoint fills in the resolved service profile's endpoint.configurable
	// slot (signal-driven-review.md §7.1/§7.2). Only meaningful — and only
	// accepted by validateServiceConfig — when Uses is set; the profile
	// itself decides whether a value is required, forbidden, or unused
	// (internal/integrationpack.DesugarService enforces the pairing since
	// this package has no access to the profile that decides it).
	Endpoint string `yaml:"endpoint,omitempty"`
	// EndpointSecretKey, when set, resolves the profile's
	// endpoint.configurable slot from the secret store instead of using
	// the literal Endpoint above — account-qualified/namespace-qualified
	// the same way BaseURLSecretKey already is for a free-form entry
	// (docs/plans/api-gateway-credential-accounts.md D13). Mutually
	// exclusive with Endpoint. Only meaningful — and only accepted by
	// validateServiceConfig — when Uses is set, mirroring Endpoint's own
	// scoping.
	//
	// Motivating case: a Pack instance (e.g. jira-cloud) declared once but
	// used from multiple workspaces, each of which is really a different
	// tenant of the same underlying product — the free-form path's D12
	// rationale (BaseURLSecretKey's own doc comment) applies identically
	// here: reuse the secret store's per-namespace value isolation rather
	// than grow the config shape with a second named instance per tenant.
	// Whether an entry actually NEEDS this (vs. the literal Endpoint) is
	// Pack-profile-dependent, same as Endpoint's own optionality —
	// internal/integrationpack.DesugarService enforces the pairing once
	// the Pack registry is known.
	EndpointSecretKey string `yaml:"endpoint_secret_key,omitempty"`
	// Credentials binds each of the resolved service profile's declared
	// credential slot names to a SecretStore key reference — never a
	// plaintext value, the same convention ServiceAuthConfig.SecretKey
	// already has (signal-driven-review.md §7.2's `credentials: {token:
	// JIRA_TOKEN}` example). Only meaningful — and only accepted by
	// validateServiceConfig — when Uses is set.
	Credentials map[string]string `yaml:"credentials,omitempty"`
	// Username fills in the resolved service profile's INSTANCE-SPECIFIC
	// Basic-auth username slot (feat/credential-slot-instance-username:
	// internal/integrationpack.CredentialSlot.UsernameFrom ==
	// UsernameFromInstance) — e.g. a Jira Cloud instance's Basic-auth
	// convention of username = the operator's own Atlassian account email,
	// which differs per instance and so cannot be a Pack-fixed constant the
	// way Bitbucket's "x-bitbucket-api-token-auth" is (CredentialSlot.
	// Username). Deliberately a TOP-LEVEL, PLAINTEXT field — mirroring
	// Endpoint's own shape, not folded into Credentials above: Credentials
	// only ever binds SecretStore KEY REFERENCES, and an email address is a
	// config value, not a secret, so mixing the two would blur the secret/
	// non-secret distinction every other field in this file preserves. Only
	// meaningful — and only accepted by validateServiceConfig — when Uses is
	// set; whether the resolved profile's slot actually accepts it (as
	// opposed to declaring its own fixed username, or using a non-basic
	// injection) is internal/integrationpack.DesugarService's job (Q16: this
	// package cannot reach the Pack registry).
	Username string `yaml:"username,omitempty"`
	// UsernameSecretKey, when set, resolves the profile's INSTANCE-SPECIFIC
	// Basic-auth username slot from the secret store instead of using the
	// literal Username above — account-qualified/namespace-qualified the
	// same way ServiceAuthConfig.UsernameSecretKey already is for a
	// free-form entry (docs/plans/api-gateway-credential-accounts.md D13).
	// Mutually exclusive with Username. Only meaningful — and only accepted
	// by validateServiceConfig — when Uses is set, mirroring Username's own
	// scoping; whether the resolved profile's slot actually accepts it (as
	// opposed to declaring its own fixed username, or using a non-basic
	// injection) is internal/integrationpack.DesugarService's job, same as
	// Username itself.
	//
	// NOT the same config path as ServiceAuthConfig.UsernameSecretKey
	// (services.<name>.auth.username_secret_key, free-form entries only) —
	// see that field's own doc comment for why the two look similar but are
	// scoped to mutually-exclusive entry shapes.
	UsernameSecretKey string `yaml:"username_secret_key,omitempty"`
}

// ParseUsesReference parses a services.<name>.uses value of the form
// "<pack名>/<profile名>@<pack version>" (docs/plans/signal-driven-review.md
// §7.2) into its three components. Exported so internal/integrationpack —
// which resolves the reference against the loaded Pack registry — parses it
// with the exact same rule this package's own load-time syntax check
// (validateServiceConfig) uses, rather than each maintaining its own
// split/regex logic that could silently drift apart on an edge case (e.g.
// how a pack name containing "@" or "/" is rejected).
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

// ValidateServiceURL validates rawURL for the safety properties every
// service's outbound upstream address must have (docs/plans/api-gateway.md
// §1/§2):
//   - an absolute URL with an explicit scheme and host
//   - no query string or fragment (apigateway.Server always forwards the
//     INBOUND request's own RawQuery verbatim and never merges in anything
//     from this URL, and an HTTP fragment is never sent to a server at all
//     — RFC 3986 §3.5)
//   - https, unless allowInsecure is true (a plain-http scheme means an
//     injected credential crosses the network to the upstream in
//     cleartext — allowInsecure moves that acknowledgment into the config
//     document itself rather than a log line that may never be read)
//   - only http/https schemes even with allowInsecure set (the gateway's
//     outbound Transport only ever speaks http/https regardless of this
//     flag — "ftp"/"ws"/anything else would validate cleanly yet fail
//     every request at 502 once dispatched)
//
// Exported and shared by validateServiceConfig's own base_url check AND
// its uses:-branch endpoint check (F1, review finding, HIGH/security: a
// uses: entry's Endpoint used to bypass every one of these checks
// entirely — malformed/http-without-allow_insecure/non-http(s)-scheme/
// query-string endpoints all loaded cleanly, silently breaking
// apigateway.NewCredentialProvider's own documented "already validated by
// config.yaml load" invariant). serviceName/fieldName only name the
// offending services.<name> entry/key in the returned error — fieldName is
// "base_url" or "endpoint" depending on the caller.
// This is now a thin wrapper around apigateway.ValidateBaseURL (docs/plans/
// api-gateway-credential-accounts.md D12) — the SAME check needs to run at
// request time too, for a services.<name>.base_url_secret_key-backed
// service whose real base_url isn't known until the secret store resolves
// it, and internal/apigateway cannot import this package back (the
// dependency runs config -> apigateway, not the reverse), so the
// implementation lives there and this function delegates rather than the
// two packages drifting apart on the same rules. Error text/wording is
// unchanged — callers/tests asserting on it are unaffected.
func ValidateServiceURL(serviceName, fieldName, rawURL string, allowInsecure bool) error {
	return apigateway.ValidateBaseURL(serviceName, fieldName, rawURL, allowInsecure)
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
	// docs/plans/api-gateway-credential-accounts.md D1: "@" in the request
	// path's service segment separates the service name from an optional
	// credential-account qualifier ("freee@ubs" — apigateway.parsePath). A
	// services.<name> config key containing "@" would be permanently
	// unreachable (parsePath would always parse everything from the first
	// "@" onward as an account, never as part of the service name) and,
	// worse, would make the two constructs ambiguous — rejected outright at
	// config-load time, the same "fail loud with the offending name" posture
	// every other leaf in this function has. Applies uniformly to a
	// uses:-backed entry too (this check runs before the uses:/base_url
	// branch below), since a Pack-profile-backed instance's name is the same
	// services.<name> map key a free-form entry's is.
	if strings.Contains(name, "@") {
		return fmt.Errorf("services[%q]: service name must not contain \"@\" — the gateway path reserves \"@\" to separate a service name from an optional credential-account qualifier (docs/plans/api-gateway-credential-accounts.md)", name)
	}

	// uses: (docs/plans/signal-driven-review.md §7.2, docs/plans/
	// signal-ingest-detailed-design.md §6.1): a Pack-profile-backed instance
	// is a completely different shape from a free-form base_url/auth entry
	// — its base_url/auth come from the resolved profile instead — so the
	// two are mutually exclusive, checked BEFORE the base_url/auth
	// requirements below ever run (a uses: entry has no reason to satisfy
	// them at all). This package can only validate uses:'s own SYNTAX
	// (ParseUsesReference) — resolving it against the installed Pack
	// registry is internal/integrationpack's job (this package does not,
	// and must not, import anything Pack-specific — Q16).
	if sc.Uses != "" {
		if sc.BaseURL != "" {
			return fmt.Errorf("services[%q]: \"uses\" and \"base_url\" are mutually exclusive — base_url comes from the resolved Integration Pack service profile instead", name)
		}
		// base_url_secret_key (docs/plans/api-gateway-credential-accounts.md
		// D12) has no meaning for a uses: entry either, for the exact same
		// reason base_url itself does not — the profile's endpoint (sc.
		// Endpoint) is the only source DesugarService ever reads a uses:
		// entry's base_url from. Rejected here rather than silently
		// ignored, matching every other stray-field-under-uses: check in
		// this function.
		if sc.BaseURLSecretKey != "" {
			return fmt.Errorf("services[%q]: \"uses\" and \"base_url_secret_key\" are mutually exclusive — base_url comes from the resolved Integration Pack service profile instead", name)
		}
		if sc.Auth != (ServiceAuthConfig{}) {
			return fmt.Errorf("services[%q]: \"uses\" and \"auth\" are mutually exclusive — credential injection comes from the resolved Integration Pack service profile instead (set \"credentials\" to bind SecretStore keys to the profile's declared slots)", name)
		}
		if _, _, _, err := ParseUsesReference(sc.Uses); err != nil {
			return fmt.Errorf("services[%q]: \"uses\": %w", name, err)
		}
		// endpoint: (F1, review finding, HIGH/security): this used to
		// `return nil` right here without ever validating sc.Endpoint at
		// all — an operator's endpoint value reached
		// apigateway.NewCredentialProvider completely unvalidated,
		// silently breaking that constructor's own documented "already
		// validated by config.yaml load" invariant (malformed/http-without-
		// allow_insecure/non-http(s)-scheme/query-string endpoints all
		// loaded cleanly). A uses:-based instance's resolved BaseURL comes
		// from Endpoint (internal/integrationpack.DesugarService), so it
		// must satisfy the exact same safety properties base_url does —
		// ValidateServiceURL is the single shared check both use. Whether
		// Endpoint is actually REQUIRED depends on the resolved service
		// profile (unknown here — Q16), so an EMPTY Endpoint is not
		// rejected by this package; DesugarService enforces that half once
		// the Pack registry is known.
		if sc.Endpoint != "" {
			if err := ValidateServiceURL(name, "endpoint", sc.Endpoint, sc.AllowInsecure); err != nil {
				return err
			}
		}
		// endpoint / endpoint_secret_key (docs/plans/api-gateway-credential-
		// accounts.md D13): mutually exclusive. Unlike base_url_secret_key
		// (which uses: forbids outright — base_url comes from Endpoint, so
		// base_url_secret_key has no meaning here), endpoint_secret_key IS
		// meaningful for a uses: entry: it is this shape's own namespace-
		// scoped-identity mechanism, parallel to base_url_secret_key's role
		// on the free-form path. Checked AFTER the literal-Endpoint
		// ValidateServiceURL call above (that call only ever fires when
		// Endpoint is non-empty, so ordering doesn't matter for correctness,
		// but mirrors D12's base_url/base_url_secret_key check placement).
		// Not validated as a URL here — that value doesn't exist at
		// config-load time; apigateway.CredentialProvider.BaseURLFor
		// resolves and validates it at request time instead (same deferral
		// base_url_secret_key already documents).
		if sc.Endpoint != "" && sc.EndpointSecretKey != "" {
			return fmt.Errorf("services[%q]: \"endpoint\" and \"endpoint_secret_key\" are mutually exclusive", name)
		}
		// username / username_secret_key (D13): mutually exclusive. Whether
		// either is actually REQUIRED depends on the resolved profile's
		// credential slot (unknown here — Q16), so neither is required by
		// this package; internal/integrationpack.DesugarService enforces
		// that half once the Pack registry is known, the same split
		// Username's own doc comment already documents.
		if sc.Username != "" && sc.UsernameSecretKey != "" {
			return fmt.Errorf("services[%q]: \"username\" and \"username_secret_key\" are mutually exclusive", name)
		}
		return nil
	}
	// endpoint:/credentials:/username: only mean anything alongside uses:
	// (they fill in / bind a resolved Pack service profile's declared slots
	// — see ServiceConfig's own doc comments) — rejecting them outright on a
	// free-form entry catches a likely stray copy-paste at config-load time
	// instead of the value silently doing nothing. username: in particular
	// is easy to confuse with auth.username (the field that actually
	// configures Basic auth on a free-form entry) — the error names both so
	// the fix is obvious.
	if sc.Endpoint != "" {
		return fmt.Errorf("services[%q]: \"endpoint\" requires \"uses\" to be set", name)
	}
	// endpoint_secret_key (D13) is the uses:-only counterpart to Endpoint —
	// same stray-field rejection, same reason.
	if sc.EndpointSecretKey != "" {
		return fmt.Errorf("services[%q]: \"endpoint_secret_key\" requires \"uses\" to be set", name)
	}
	if len(sc.Credentials) > 0 {
		return fmt.Errorf("services[%q]: \"credentials\" requires \"uses\" to be set", name)
	}
	if sc.Username != "" {
		return fmt.Errorf("services[%q]: \"username\" requires \"uses\" to be set (for a free-form entry, use \"auth.username\" instead)", name)
	}
	// username_secret_key (D13) is the uses:-only counterpart to the
	// top-level Username — same stray-field rejection, same reason; the
	// free-form equivalent lives at auth.username_secret_key instead (D12).
	if sc.UsernameSecretKey != "" {
		return fmt.Errorf("services[%q]: \"username_secret_key\" requires \"uses\" to be set (for a free-form entry, use \"auth.username_secret_key\" instead)", name)
	}

	// base_url / base_url_secret_key (docs/plans/api-gateway-credential-
	// accounts.md D12): mutually exclusive, exactly one required for a
	// free-form entry.
	if sc.BaseURL == "" && sc.BaseURLSecretKey == "" {
		return fmt.Errorf("services[%q]: missing required \"base_url\" (or \"base_url_secret_key\") field", name)
	}
	if sc.BaseURL != "" && sc.BaseURLSecretKey != "" {
		return fmt.Errorf("services[%q]: \"base_url\" and \"base_url_secret_key\" are mutually exclusive", name)
	}
	if sc.BaseURL != "" {
		// Parsed and scheme/host-checked here, at config-load time, rather
		// than left for apigateway.NewCredentialProvider's own defensive
		// parse to discover later (internal/server/wire.go). That
		// constructor's doc comment states an "already validated by config
		// load" invariant for exactly this reason — a malformed base_url
		// should fail `boid start` loudly (the same posture
		// gateway.forges.*.host gets), not silently vanish from the
		// gateway's service registry with only a log warning once the
		// daemon is already running. ValidateServiceURL is the exact same
		// check the uses: branch's own "endpoint" field above shares (F1).
		//
		// This invariant does NOT extend to base_url_secret_key (D12): that
		// value doesn't exist at config-load time — it lives in the secret
		// store, resolved and validated by
		// apigateway.CredentialProvider.BaseURLFor at request time instead
		// (ValidateBaseURL — the same shared implementation this call
		// delegates to). A malformed base_url_secret_key VALUE therefore
		// fails at request time (502), not at `boid start` — an accepted,
		// necessary trade-off of deferring the value to the secret store.
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
		// auth.username / auth.username_secret_key (D12): mutually
		// exclusive, exactly one required.
		if sc.Auth.Username == "" && sc.Auth.UsernameSecretKey == "" {
			return fmt.Errorf("services[%q]: auth.kind basic requires \"username\" (or \"username_secret_key\")", name)
		}
		if sc.Auth.Username != "" && sc.Auth.UsernameSecretKey != "" {
			return fmt.Errorf("services[%q]: auth.username and auth.username_secret_key are mutually exclusive", name)
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
		//
		// Only checked for the literal Username here — a
		// UsernameSecretKey-backed value doesn't exist at config-load time
		// (D12); CredentialProvider.resolveUsername re-applies this exact
		// check once the secret store resolves it, at request time.
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
			return fmt.Errorf("services[%q]: auth.kind oauth2 requires \"provider\" (docs/plans/api-gateway.md §論点4 — names an oauth_providers.<name> entry)", name)
		}
		// Provider is deliberately NOT cross-validated against
		// oauth_providers here — see
		// TestLoadFromPath_Services_OAuth2ProviderReferenceNotCrossValidated's
		// own doc comment (oauth_providers_test.go) for the full reasoning:
		// this mirrors every other secret-store-shaped reference in this
		// file (auth.secret_key included), none of which is validated
		// against its actual target at config-load time either.
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
//
// A uses: entry (docs/plans/signal-driven-review.md §7.2) is deliberately
// EXCLUDED from the output — it has no BaseURL/Auth of its own to convert
// (those come from the resolved Integration Pack service profile, which
// this package cannot reach — Q16: core must not import anything
// Pack-specific). internal/integrationpack.ResolveServices is what combines
// this method's output with its own Pack-resolved uses: entries into the
// full flat list internal/server/wire.go's gateway wiring point actually
// needs — see that function's own doc comment.
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
// deliberately excludes them, see that method's own doc comment) that
// internal/integrationpack.ResolveServices consumes to know which
// instances to desugar against the loaded Pack registry. Each entry has
// already passed validateServiceConfig's own uses:-vs-base_url/auth
// exclusivity and uses: syntax checks (Config.UnmarshalYAML validates every
// c.Services entry eagerly at load time) — resolving the reference against
// installed Packs is the one thing this package cannot do itself (Q16).
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
// entry's `provider` field references (docs/plans/api-gateway.md §6/§論点4:
// "config.yaml の oauth_providers: ブロック。client_secret のみ SecretStore
// 参照"). PR2 only ever performed a refresh_token grant against
// TokenEndpoint; PR3 adds Flow/AuthorizationEndpoint/
// DeviceAuthorizationEndpoint/AuthorizeParams for `boid secret oauth login
// <service>` (docs/plans/api-gateway.md §7 "初回認証").
type OAuthProviderConfig struct {
	// TokenEndpoint is the provider's OAuth2 token endpoint (RFC 6749
	// §3.2), e.g. https://accounts.secure.freee.co.jp/public_api/token.
	// Always https — see validateOAuthProviderConfig's own doc comment for
	// why there is no allow_insecure-style escape hatch here.
	TokenEndpoint string `yaml:"token_endpoint"`
	// ClientID is the OAuth2 client_id. Not a secret by RFC 6749 §2.2's own
	// classification (a public identifier), so — unlike ClientSecretKey —
	// it is written directly here rather than referenced through the
	// secret store.
	ClientID string `yaml:"client_id"`
	// ClientSecretKey is a secret-store key reference (never a plaintext
	// value) for a confidential client's client_secret (docs/plans/
	// api-gateway.md §7: "confidential client の client_secret を daemon 側
	// にのみ置く"). Empty for a public client (PKCE, no client_secret) —
	// apigateway.OAuth2TokenSource simply omits client_secret from the
	// token request when this is empty.
	ClientSecretKey string `yaml:"client_secret_key,omitempty"`
	// Scopes is consumed by `boid secret oauth login`'s authorization-URL
	// construction (loopback/manual) and device-authorization request
	// (device) — docs/plans/api-gateway.md §7, PR3. Never sent on an
	// authorization_code refresh_token grant; see
	// apigateway.OAuth2TokenSource.callRefreshTokenEndpoint's own doc comment
	// for why. A client_credentials grant (§6-補, PR4) DOES send Scopes on
	// every token request — see callClientCredentialsTokenEndpoint instead.
	Scopes []string `yaml:"scopes,omitempty"`

	// Flow selects which of the three initial-grant flows (docs/plans/
	// api-gateway.md §7 "flow の三段構え", PR3) `boid secret oauth login
	// <service>` uses for this provider: "device" / "loopback" / "manual".
	// Empty ("") means this provider has no login flow configured — an
	// operator can still seed refresh_token by hand via `boid secret set`
	// (PR2's own documented fallback), but `boid secret oauth login` for it
	// fails with a clear "no flow configured" error rather than guessing.
	// Deliberately OPTIONAL (not required on every entry): an existing
	// config.yaml written against PR2 alone — token_endpoint/client_id/
	// client_secret_key only, refresh_token seeded by hand — must keep
	// loading without modification once PR3 ships.
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
	// apigateway.OAuthProviderConfig.AuthorizeParams' own doc comment for
	// the motivating example (Google's access_type/prompt). Keys colliding
	// with a protocol-reserved parameter are rejected below.
	AuthorizeParams map[string]string `yaml:"authorize_params,omitempty"`

	// Grant selects which RFC 6749 grant apigateway.OAuth2TokenSource.
	// refresh performs for this provider (docs/plans/api-gateway.md §6-補):
	// "authorization_code" (the default — every 3-legged/delegated flow
	// this package supported before §6-補) or "client_credentials" (RFC
	// 6749 §4.4, 2-legged / app-only, Service Principal). Deliberately
	// OPTIONAL and a SEPARATE field from Flow, not a fourth Flow value —
	// see apigateway.OAuthProviderConfig.Grant's own doc comment for why.
	// Empty ("") means "authorization_code", so an existing PR2/PR3
	// config.yaml (written before this field existed) keeps loading
	// unmodified.
	Grant string `yaml:"grant,omitempty"`
}

// reservedAuthorizeParamNames is every query/form parameter this package's
// own login-flow construction (apigateway/login.go's buildAuthorizeURL /
// postDeviceAuthorizationRequest) sets itself — an operator-supplied
// authorize_params entry using one of these keys would silently either be
// overwritten by (loopback/manual: url.Values.Set in buildAuthorizeURL
// applies AuthorizeParams AFTER the protocol fields, so an operator's
// value would actually WIN, corrupting the PKCE/state/redirect_uri
// machinery those functions depend on) or collide unpredictably with the
// protocol machinery itself, so config-load rejects it outright instead —
// the same "fail loud with the offending name" posture every other leaf in
// this file has.
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
// plain string here, an apigateway.LoginFlow there — see that type's own
// doc comment for why the two must never silently drift apart).
var validLoginFlows = map[string]bool{
	string(apigateway.LoginFlowDevice):   true,
	string(apigateway.LoginFlowLoopback): true,
	string(apigateway.LoginFlowManual):   true,
}

// validOAuthGrants mirrors apigateway.ValidOAuthGrants exactly (Grant is a
// plain string here, an apigateway.OAuthGrant there — see that type's own
// doc comment for why the two must never silently drift apart).
var validOAuthGrants = map[string]bool{
	string(apigateway.GrantAuthorizationCode): true,
	string(apigateway.GrantClientCredentials): true,
}

// validateOAuthProviderConfig validates one oauth_providers.<name> entry,
// mirroring validateServiceConfig's "fail loud with the offending name"
// posture.
func validateOAuthProviderConfig(name string, pc OAuthProviderConfig) error {
	if trimmed := strings.TrimSpace(name); trimmed != name {
		return fmt.Errorf("oauth_providers[%q]: provider name must not have leading/trailing whitespace (did you mean %q?)", name, trimmed)
	}
	if name == "" {
		return fmt.Errorf("oauth_providers: a provider name must not be empty")
	}
	// docs/plans/api-gateway-credential-accounts.md D1: PR-2's account-
	// qualified oauth2 credential key is "oauth2:<provider>@<account>:...",
	// so a provider name containing "@" would collide with that separator —
	// rejected here for the same reason services.<name> rejects it above
	// (validateServiceConfig).
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
	// provider call (docs/plans/api-gateway.md's targeted providers —
	// freee, Google, GitHub, Microsoft, Atlassian — all require https over
	// the public internet), never a sandbox-facing or internal-test-API
	// endpoint the way services.*.base_url can legitimately be. There is no
	// PR1-style plaintext-internal-service use case to support here, so
	// this is a hard requirement rather than an opt-out-able one.
	if u.Scheme != "https" {
		return fmt.Errorf("oauth_providers[%q]: \"token_endpoint\" scheme must be https (got %q) — an OAuth2 token endpoint always requires TLS", name, u.Scheme)
	}
	if pc.ClientID == "" {
		return fmt.Errorf("oauth_providers[%q]: missing required \"client_id\" field", name)
	}

	if pc.Grant != "" && !validOAuthGrants[pc.Grant] {
		return fmt.Errorf("oauth_providers[%q]: \"grant\": unrecognized %q (want one of authorization_code, client_credentials)", name, pc.Grant)
	}
	// client_credentials-specific requirements (docs/plans/api-gateway.md
	// §6-補, "設計方針"): checked here, eagerly, at config load, the same
	// "fail loud with the offending name" posture every other
	// flow/grant-conditional requirement in this function already has.
	if apigateway.OAuthGrant(pc.Grant) == apigateway.GrantClientCredentials {
		// RFC 6749 §4.4.2: client_credentials requires a confidential
		// client. This catches an UNSET client_secret_key; a client_secret_
		// key that IS set but resolves to an empty secret-store VALUE
		// cannot be caught here (config load has no secret-store access at
		// all) — apigateway.OAuth2TokenSource.refreshClientCredentials
		// catches that half at refresh time instead (see its own doc
		// comment for why).
		if pc.ClientSecretKey == "" {
			return fmt.Errorf("oauth_providers[%q]: \"client_secret_key\" is required when \"grant\" is client_credentials (RFC 6749 §4.4.2 requires a confidential client)", name)
		}
		// grant and flow are a deliberately separate axis (apigateway.
		// OAuthProviderConfig.Grant's own doc comment) — client_credentials
		// has no login flow at all (no user authorization step exists to
		// select a flow for), so the two must never be set together.
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
// daemon-to-real-OAuth2-provider call, never an internal test/staging
// service.
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
// consumes (internal/server/wire.go), sorted by name for determinism.
// Mirrors APIGatewayServices' own shape and "already validated by
// Config.UnmarshalYAML" invariant — an entry that fails
// validateOAuthProviderConfig here (a hand-built Config that skipped
// validation) is skipped silently rather than surfaced as an error, since
// this method has no error return and never should have one under that
// invariant.
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
