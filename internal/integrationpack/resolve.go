package integrationpack

import (
	"fmt"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/config"
)

// findPack returns the *Pack among packs whose Name/Version match pack/
// version exactly, or (nil, false).
func findPack(packs []*Pack, pack, version string) (*Pack, bool) {
	for _, p := range packs {
		if p.Name == pack && p.Version == version {
			return p, true
		}
	}
	return nil, false
}

// DesugarService resolves one services.<name> config entry — sc.Uses must
// be non-empty — against packs, producing the apigateway.ServiceConfig the
// gateway registry sees for it (the "脱糖", desugaring, of a uses:
// reference). instanceName becomes the returned ServiceConfig's Name (and
// appears in every error message this function returns).
//
// Resolution order, each step a hard error naming instanceName:
//  1. sc.Uses parses as "<pack>/<profile>@<version>" (config.
//     ParseUsesReference — already checked once at config-load time by
//     config.validateServiceConfig; re-checked here since this function may
//     be called with a hand-built config.ServiceConfig that skipped that
//     validation, e.g. in a test).
//  2. pack/version must match an entry in packs.
//  3. profile must be declared in that pack's manifest.
//  4. sc.Endpoint and sc.EndpointSecretKey must not both be set (mutually
//     exclusive, the same "already checked once at config-load time,
//     re-checked here" posture item 1 above has). The profile's endpoint
//     requirement must then match what sc provides: if Endpoint.Configurable,
//     at least one of sc.Endpoint/sc.EndpointSecretKey must be non-empty;
//     otherwise v0 has no Pack-declared default endpoint to fall back to
//     (see ServiceProfile.Endpoint's own doc comment), so ANY use of such a
//     profile is rejected in v0.
//  5. the profile must declare exactly one credential slot, sc.Credentials
//     must bind it, and must not carry any other key.
//  6. sc.Username and sc.UsernameSecretKey must not both be set. If that
//     slot's injection is basic and it declares usernameFrom: instance,
//     exactly one of sc.Username (non-empty and colon-free, RFC 7617 §2,
//     checked here) or sc.UsernameSecretKey (colon-checked later instead,
//     at request time, once the secret store resolves it) must be set; if
//     the slot does NOT declare usernameFrom: instance (a Pack-fixed
//     username, or any non-basic injection), neither may be set — there is
//     no slot for an instance-supplied value to fill.
//
// The returned apigateway.ServiceConfig's AllowReadOnlyWrite is copied
// straight from sc.AllowReadOnlyWrite: a Pack-profile-backed instance opts
// into the read-only→GET/HEAD-only gate exactly the same way a free-form
// base_url/auth entry does via config.APIGatewayServices().
func DesugarService(instanceName string, sc config.ServiceConfig, packs []*Pack) (apigateway.ServiceConfig, error) {
	packName, profileName, version, err := config.ParseUsesReference(sc.Uses)
	if err != nil {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: uses: %w", instanceName, err)
	}

	pack, ok := findPack(packs, packName, version)
	if !ok {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: uses %q references pack %q@%q, which is not installed", instanceName, sc.Uses, packName, version)
	}
	profile, ok := pack.ServiceProfile(profileName)
	if !ok {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: uses %q references profile %q, which pack %q@%q does not declare", instanceName, sc.Uses, profileName, packName, version)
	}

	// endpoint / endpoint_secret_key: config.validateServiceConfig already
	// rejects setting both at config-load time — re-checked here
	// defensively, the same posture every other check in this function has
	// (see this function's own doc comment, item 1).
	if sc.Endpoint != "" && sc.EndpointSecretKey != "" {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: \"endpoint\" and \"endpoint_secret_key\" are mutually exclusive", instanceName)
	}
	if profile.Endpoint.Configurable {
		if sc.Endpoint == "" && sc.EndpointSecretKey == "" {
			return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: profile %q requires \"endpoint\" (or \"endpoint_secret_key\") to be set", instanceName, profileName)
		}
	} else if sc.Endpoint != "" || sc.EndpointSecretKey != "" {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: profile %q does not accept \"endpoint\" or \"endpoint_secret_key\" (endpoint.configurable is false, and v0 has no Pack-declared default endpoint to fall back to)", instanceName, profileName)
	} else {
		// profile.Endpoint.Configurable == false and sc.Endpoint == "": v0
		// has no default-endpoint mechanism (ServiceProfile.Endpoint's own
		// doc comment), so this profile cannot be used at all yet.
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: profile %q declares endpoint.configurable: false, which v0 does not yet support for uses: (no Pack-declared default endpoint mechanism exists) — the profile must declare endpoint.configurable: true", instanceName, profileName)
	}

	if len(profile.Credentials) != 1 {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: profile %q declares %d credential slots, v0 requires exactly 1 to desugar a uses: entry", instanceName, profileName, len(profile.Credentials))
	}
	slot := profile.Credentials[0]

	for key := range sc.Credentials {
		if key != slot.Name {
			return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: credentials: %q is not a slot profile %q declares (want %q)", instanceName, key, profileName, slot.Name)
		}
	}
	secretKey, bound := sc.Credentials[slot.Name]
	if !bound || secretKey == "" {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: credentials: profile %q's slot %q is not bound (set credentials.%s to a SecretStore key)", instanceName, profileName, slot.Name, slot.Name)
	}

	// username / username_secret_key: config.validateServiceConfig already
	// rejects setting both at config-load time — re-checked here
	// defensively, same posture as the endpoint/endpoint_secret_key check
	// above.
	if sc.Username != "" && sc.UsernameSecretKey != "" {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: \"username\" and \"username_secret_key\" are mutually exclusive", instanceName)
	}
	// The slot's own Username (a Pack-fixed constant, e.g. Bitbucket's
	// "x-bitbucket-api-token-auth") is the default; usernameFrom: instance
	// overrides that with EITHER of the two values config.yaml's uses:
	// entry can supply instead (literal Username, or UsernameSecretKey,
	// resolved from the secret store per namespace/account at request
	// time). Either way exactly one Pack-declared source is authoritative
	// per slot (parseCredentialSlot already enforces that a basic slot
	// declares exactly one of Username/UsernameFrom) — what THIS function
	// enforces is that the instance's username config agrees with which
	// source the slot actually declared: present (as literal OR
	// secret-key) when required, absent when there is no slot for it to
	// fill.
	username := slot.Username
	usernameSecretKey := ""
	if slot.Injection == InjectionBasic && slot.UsernameFrom == UsernameFromInstance {
		switch {
		case sc.Username == "" && sc.UsernameSecretKey == "":
			return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: profile %q's credential slot %q requires the service instance to supply \"username\" or \"username_secret_key\" (usernameFrom: instance) — set services.%s.username or services.%s.username_secret_key", instanceName, profileName, slot.Name, instanceName, instanceName)
		case sc.UsernameSecretKey != "":
			// Deferred to request time (apigateway.CredentialProvider.
			// resolveUsername) — a secret-store value doesn't exist yet at
			// desugar time, so `username` stays "" here and the RFC 7617
			// colon check below is skipped for it (resolveUsername
			// re-applies that exact check once the secret store resolves it).
			usernameSecretKey = sc.UsernameSecretKey
			username = ""
		default:
			username = sc.Username
		}
	} else if sc.Username != "" || sc.UsernameSecretKey != "" {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: \"username\" (or \"username_secret_key\") is not accepted — profile %q's credential slot %q does not declare usernameFrom: instance (it has a Pack-fixed username, or uses a non-basic injection)", instanceName, profileName, slot.Name)
	}
	// RFC 7617 §2: the Basic credential is built as "username:secret", so a
	// colon in the username changes what the upstream parses as each half —
	// the same check config.validateServiceConfig already applies to a
	// free-form auth.username entry. Checked once, here, against the FINAL
	// resolved LITERAL username regardless of its source: neither this
	// package nor config.validateServiceConfig otherwise validates a
	// Pack-fixed slot.Username, since that value never flows through a
	// free-form auth.username field. Skipped when usernameSecretKey is
	// set — there is no literal value yet to inspect; see the comment
	// above.
	if usernameSecretKey == "" && slot.Injection == InjectionBasic && strings.Contains(username, ":") {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: username %q must not contain \":\" (RFC 7617 §2 — the Basic credential is built as \"username:secret\", so a colon in the username changes what the upstream parses as each half)", instanceName, username)
	}

	return apigateway.ServiceConfig{
		Name: instanceName,
		// Mirrors config.APIGatewayServices()'s own BaseURL/BaseURLSecretKey
		// branch for the free-form path: a uses: entry's upstream identity
		// comes from sc.Endpoint (literal) or sc.EndpointSecretKey
		// (secret-backed), never both (checked above). Exactly one of
		// BaseURL/BaseURLSecretKey is non-empty here.
		BaseURL:          sc.Endpoint,
		BaseURLSecretKey: sc.EndpointSecretKey,
		// Needed by apigateway.CredentialProvider.BaseURLFor's secret-backed
		// resolution path whenever this instance sets EndpointSecretKey
		// instead of the literal Endpoint above — the resolved URL's scheme
		// still has to satisfy the same https-unless-allow_insecure rule
		// ValidateBaseURL enforces for a literal one, just at request time
		// instead of config-load time. Propagated straight from
		// sc.AllowInsecure so the two ServiceConfig structs stay in sync on
		// every shared field — TestServiceConfigFieldPropagation_Exhaustive
		// (field_propagation_exhaustive_test.go) enforces this.
		AllowInsecure: sc.AllowInsecure,
		Auth: apigateway.ServiceAuth{
			Kind:      apigateway.AuthKind(slot.Injection),
			SecretKey: secretKey,
			Username:  username,
			// Mirrors Username's own dual-source treatment just above:
			// exactly one of Username/UsernameSecretKey is non-empty when
			// the slot requires an instance-supplied username at all; both
			// are "" for a Pack-fixed username or a non-basic injection.
			UsernameSecretKey: usernameSecretKey,
			Header:            slot.Header,
			Query:             slot.Query,
		},
		AllowReadOnlyWrite: sc.AllowReadOnlyWrite,
		// Copied straight from sc.RequireAccount, the same "propagate the
		// flat bool through unchanged" treatment AllowReadOnlyWrite above
		// gets — a uses: entry opts into requiring an account qualifier
		// exactly the same way a free-form base_url/auth entry does via
		// config.APIGatewayServices().
		RequireAccount: sc.RequireAccount,
	}, nil
}

// ResolveServices returns the full flat []apigateway.ServiceConfig list for
// cfg.Services: free-form entries (base_url/auth) pass through
// cfg.APIGatewayServices() unchanged, and uses: entries (config.
// Config.UsesServices()) are desugared against packs via DesugarService.
// This is the single function internal/server/wire.go's gateway wiring
// point should call in place of cfg.APIGatewayServices() alone, once wired.
//
// Unlike APIGatewayServices() (which silently skips an entry that fails its
// own, unrelated validation — see that method's "already validated by
// Config.UnmarshalYAML" invariant), a uses: entry that fails to desugar
// FAILS ResolveServices as a whole: an unresolvable Pack reference is an
// installation/config error the daemon should refuse to start with, not a
// service quietly missing from the registry.
func ResolveServices(cfg *config.Config, packs []*Pack) ([]apigateway.ServiceConfig, error) {
	out := cfg.APIGatewayServices()

	uses := cfg.UsesServices()
	names := make([]string, 0, len(uses))
	for name := range uses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		desugared, err := DesugarService(name, uses[name], packs)
		if err != nil {
			return nil, err
		}
		out = append(out, desugared)
	}
	return out, nil
}
