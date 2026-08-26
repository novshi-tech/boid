package integrationpack

import (
	"fmt"
	"sort"

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

	return apigateway.ServiceConfig{
		Name:    instanceName,
		BaseURL: sc.Endpoint,
		Auth: apigateway.ServiceAuth{
			Kind:      apigateway.AuthKind(slot.Injection),
			SecretKey: secretKey,
			Username:  slot.Username,
			Header:    slot.Header,
			Query:     slot.Query,
		},
		AllowReadOnlyWrite: sc.AllowReadOnlyWrite,
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
