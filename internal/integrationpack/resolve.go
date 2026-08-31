package integrationpack

import (
	"fmt"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/config"
)

// findPack returns the *Pack among packs whose Name/Version match pack/
// version exactly, or (nil, false) — the "pack 不在" check docs/plans/
// signal-ingest-detailed-design.md §6.2 item 3 requires.
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
// gateway registry sees for it: the "脱糖" (desugaring)
// docs/plans/signal-ingest-detailed-design.md §6.2 item 2 describes.
// instanceName becomes the returned ServiceConfig's Name (and appears in
// every error message this function returns).
//
// Resolution order, each step a hard error naming instanceName (docs/plans/
// signal-ingest-detailed-design.md §6.2 item 3 — "起動エラー"):
//  1. sc.Uses parses as "<pack>/<profile>@<version>" (config.
//     ParseUsesReference — already checked once at config-load time by
//     config.validateServiceConfig; re-checked here since this function may
//     be called with a hand-built config.ServiceConfig that skipped that
//     validation, e.g. in a test).
//  2. pack/version must match an entry in packs ("pack 不在").
//  3. profile must be declared in that pack's manifest.
//  4. the profile's endpoint requirement must match what sc provides: if
//     Endpoint.Configurable, sc.Endpoint must be non-empty; otherwise v0 has
//     no Pack-declared default endpoint to fall back to (see
//     ServiceProfile.Endpoint's own doc comment), so ANY use of such a
//     profile is rejected in v0 — "endpoint 要求違反" covers both
//     directions.
//  5. the profile must declare exactly one credential slot, sc.Credentials
//     must bind it ("slot 未 bind"), and must not carry any OTHER key
//     ("未知 slot").
//  6. (feat/credential-slot-instance-username) if that slot's injection is
//     basic and it declares usernameFrom: instance, sc.Username must be
//     non-empty and colon-free (RFC 7617 §2); if the slot does NOT declare
//     usernameFrom: instance (a Pack-fixed username, or any non-basic
//     injection), sc.Username must be empty — there is no slot for an
//     instance-supplied value to fill.
//
// The returned apigateway.ServiceConfig's AllowReadOnlyWrite is copied
// straight from sc.AllowReadOnlyWrite (F2, review finding, MEDIUM: an
// earlier version of this function silently dropped it, and its own doc
// comment here incorrectly claimed ServiceConfig had no such field for a
// uses: entry — config.ServiceConfig.AllowReadOnlyWrite lives on the exact
// same struct a uses: entry uses; the earlier bug was that this function
// never read it, not that it does not exist). A Pack-profile-backed
// instance opts into the read-only→GET/HEAD-only gate exactly the same way
// a free-form base_url/auth entry does via config.APIGatewayServices().
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

	if profile.Endpoint.Configurable {
		if sc.Endpoint == "" {
			return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: profile %q requires \"endpoint\" to be set", instanceName, profileName)
		}
	} else if sc.Endpoint != "" {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: profile %q does not accept \"endpoint\" (endpoint.configurable is false, and v0 has no Pack-declared default endpoint to fall back to)", instanceName, profileName)
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

	// username: (feat/credential-slot-instance-username) — the slot's own
	// Username (a Pack-fixed constant, e.g. Bitbucket's
	// "x-bitbucket-api-token-auth") is the default; usernameFrom: instance
	// overrides that with the value config.yaml's uses: entry supplies
	// instead. Either way exactly one source is authoritative per slot
	// (parseCredentialSlot already enforces that a basic slot declares
	// exactly one of Username/UsernameFrom) — what THIS function enforces is
	// that sc.Username agrees with which source the slot actually declared:
	// present when required, absent when there is no slot for it to fill.
	username := slot.Username
	if slot.Injection == InjectionBasic && slot.UsernameFrom == UsernameFromInstance {
		if sc.Username == "" {
			return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: profile %q's credential slot %q requires the service instance to supply \"username\" (usernameFrom: instance) — set services.%s.username", instanceName, profileName, slot.Name, instanceName)
		}
		username = sc.Username
	} else if sc.Username != "" {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: \"username\" is not accepted — profile %q's credential slot %q does not declare usernameFrom: instance (it has a Pack-fixed username, or uses a non-basic injection)", instanceName, profileName, slot.Name)
	}
	// RFC 7617 §2: the Basic credential is built as "username:secret", so a
	// colon in the username changes what the upstream parses as each half —
	// the same check config.validateServiceConfig already applies to a
	// free-form auth.username entry. Checked once, here, against the FINAL
	// resolved username regardless of its source (F2, codex/Opus review
	// finding on PR #1017: checking only inside the usernameFrom: instance
	// branch above left a Pack-declared FIXED username (slot.Username)
	// completely unchecked — neither this package nor config.
	// validateServiceConfig ever validates that value, since it only ever
	// sees a free-form auth.username, never a uses:-resolved Pack-fixed
	// one).
	if slot.Injection == InjectionBasic && strings.Contains(username, ":") {
		return apigateway.ServiceConfig{}, fmt.Errorf("integrationpack: service %q: username %q must not contain \":\" (RFC 7617 §2 — the Basic credential is built as \"username:secret\", so a colon in the username changes what the upstream parses as each half)", instanceName, username)
	}

	return apigateway.ServiceConfig{
		Name:    instanceName,
		BaseURL: sc.Endpoint,
		// AllowInsecure (docs/plans/api-gateway-credential-accounts.md D12):
		// a uses: entry's BaseURL above is always the literal sc.Endpoint —
		// never secret-backed — so apigateway.CredentialProvider.BaseURLFor
		// never actually consults this field for a uses:-derived service
		// (that only happens on the BaseURLSecretKey path, which a uses:
		// entry can never take — validateServiceConfig rejects setting
		// base_url_secret_key alongside uses:). Propagated anyway, straight
		// from sc.AllowInsecure (already used above to validate sc.Endpoint
		// via ValidateServiceURL), so the two ServiceConfig structs stay in
		// sync on every shared field — TestServiceConfigFieldPropagation_
		// Exhaustive (field_propagation_exhaustive_test.go) enforces this.
		AllowInsecure: sc.AllowInsecure,
		Auth: apigateway.ServiceAuth{
			Kind:      apigateway.AuthKind(slot.Injection),
			SecretKey: secretKey,
			Username:  username,
			Header:    slot.Header,
			Query:     slot.Query,
		},
		AllowReadOnlyWrite: sc.AllowReadOnlyWrite,
		// RequireAccount (docs/plans/api-gateway-credential-accounts.md D5):
		// copied straight from sc.RequireAccount, the same "propagate the
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
// point should call in place of cfg.APIGatewayServices() alone, once wired
// (PR-5 or a follow-up — see this package's doc.go for why that wiring is
// out of PR-4's own scope) — the interface PR-5 builds derived triggers and
// connector job wiring against.
//
// Unlike APIGatewayServices() (which silently skips an entry that fails its
// own, unrelated validation — see that method's "already validated by
// Config.UnmarshalYAML" invariant), a uses: entry that fails to desugar
// FAILS ResolveServices as a whole: an unresolvable Pack reference is an
// installation/config error the daemon should refuse to start with, not a
// service quietly missing from the registry (docs/plans/
// signal-ingest-detailed-design.md §6.2 item 3 — "起動エラー").
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
