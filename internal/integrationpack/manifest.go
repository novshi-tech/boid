package integrationpack

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ManifestAPIVersion is the only apiVersion ParseManifest accepts
// (docs/plans/signal-driven-review.md §6.3, docs/plans/
// signal-ingest-detailed-design.md §7.3). A manifest declaring any other
// value — a typo, or a genuinely newer Pack contract version this binary
// predates — is rejected outright: the contract's own evolution rule (§7.3)
// states boid never "前方互換を装う" (pretends forward compatibility) with a
// Pack contract version it does not implement.
const ManifestAPIVersion = "boid.dev/v1"

// ManifestKind is the only kind ParseManifest accepts.
const ManifestKind = "IntegrationPack"

// skillNamePattern is skills[].name's allowlist — see ParseManifest's use of
// it for why this is a positive allowlist rather than another entry in a
// denylist. Chosen to match ordinary directory-name conventions (what every
// skills[].name in the three official Packs already looks like:
// "jira-api", "bitbucket-api", "slack-api") rather than to be maximally
// permissive: must start with a letter or digit (so a leading "-" cannot be
// mistaken for a flag by anything that later shells out with the name
// unquoted), and every character after that is a letter, digit, ".", "_",
// or "-".
var skillNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// packPathPattern is skills[].path and connectors[].executable's allowlist,
// applied ON TOP OF filepath.IsLocal rather than instead of it (IsLocal
// still does the real "cannot escape the Pack directory" work — this only
// narrows what characters a path SEGMENT may contain). Same lesson
// skillNamePattern's own doc comment describes, applied here before it
// could repeat as a second denylist-of-the-week: a byte a downstream shell
// strips or reinterprets (NUL being the concrete case B3 found, but a
// denylist there would just start the same cycle over) can turn a
// textually-local sequence of segments into something that resolves
// differently once whatever consumes it actually runs it. Both fields
// reach a shell exactly the way skills[].name does — Skill.Path becomes
// SkillLink.Target, joined onto pack.Dir and handed to `ln -sfn` in
// buildWorkspaceInitScript; Connector.Executable becomes
// BOID_CONNECTOR_EXEC, exec'd by the job's own shell (internal/server's
// connector_exec.go) — so both get the same allowlist treatment name did,
// not a separate ad hoc fix.
var packPathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// ValidSkillName reports whether name matches skills[].name's allowlist —
// exported so a consumer of already-loaded Packs (internal/dispatcher's
// skillLinks, which turns Skill.Name into a <discovery root>/<Name>
// symlink basename) can apply the SAME check as defense in depth, rather
// than reimplementing a second, potentially-divergent one. ParseManifest is
// the primary gate (a manifest failing this fails daemon startup outright);
// this export exists for the consumer that cannot assume every *Pack it is
// ever handed came through ParseManifest (a test fixture, a future
// constructor).
func ValidSkillName(name string) bool {
	return skillNamePattern.MatchString(name)
}

// CredentialInjection selects how a service profile's credential slot is
// injected into a proxied request — a v0 subset of apigateway.AuthKind
// (docs/plans/signal-ingest-detailed-design.md §6.2: "injection:
// bearer|basic|header|query"). AuthOAuth2 is deliberately absent: a
// Pack-declared OAuth2 profile remains a §12 open question in
// signal-driven-review.md, not something PR-4 implements.
type CredentialInjection string

const (
	InjectionBearer CredentialInjection = "bearer"
	InjectionBasic  CredentialInjection = "basic"
	InjectionHeader CredentialInjection = "header"
	InjectionQuery  CredentialInjection = "query"
)

// Manifest is one Pack's strict-decoded, structurally-validated
// integration.yaml (docs/plans/signal-driven-review.md §6.3). Produced by
// ParseManifest; LoadPacks (pack.go) additionally cross-checks Metadata
// against the directory it was installed under.
type Manifest struct {
	APIVersion      string
	Kind            string
	Metadata        ManifestMetadata
	ServiceProfiles []ServiceProfile
	Connectors      []Connector
	Skills          []Skill
}

// ManifestMetadata is the manifest's metadata: block.
type ManifestMetadata struct {
	Name    string
	Version string
}

// ServiceProfile is one serviceProfiles[] entry — "接続の型" (§7.1): the
// credential slot(s) and endpoint requirement a service instance
// (config.yaml services.<name>) must satisfy to use this profile. Never
// carries a value of its own (§7.1: "値 (endpoint 実値・credential) を持た
// ない") — see DesugarService for where instance values get bound.
type ServiceProfile struct {
	Name string
	// Endpoint declares whether an instance may/must supply an endpoint
	// value. v0 has no Pack-declared DEFAULT endpoint field (signal-driven-
	// review.md §7.1's "endpoint 固定のサービス (Slack) は configurable:
	// false + 既定値を宣言する" future extension is not implemented here) —
	// see DesugarService's own doc comment for the resulting v0 limitation.
	Endpoint EndpointSpec
	// Credentials is the profile's declared credential slot(s). v0 requires
	// AT MOST one — LoadPacks rejects a profile declaring more than one
	// outright (docs/plans/signal-ingest-detailed-design.md §6.2: "先頭
	// slot を勝手に取るような縮退をしない"). Zero slots is structurally
	// allowed (an unauthenticated public API profile), but DesugarService
	// has nothing to bind for such a profile in v0 — see its own doc
	// comment.
	Credentials []CredentialSlot
}

// EndpointSpec is a service profile's endpoint.
type EndpointSpec struct {
	// Configurable, when true, means an instance MUST supply Endpoint
	// (DesugarService rejects a uses: entry that omits it). When false (the
	// zero value — also what an omitted endpoint: block means), an instance
	// must NOT supply one — v0 has no default-endpoint mechanism to fall
	// back to, so DesugarService rejects any use of such a profile
	// altogether (see its own doc comment for the full reasoning).
	Configurable bool
}

// CredentialSlot is one profile credential slot declaration.
type CredentialSlot struct {
	Name      string
	Injection CredentialInjection
	// Header is the header name to set — required (and only meaningful)
	// when Injection == InjectionHeader, mirroring config.ServiceAuthConfig.Header.
	Header string
	// Query is the query parameter name to set — required (and only
	// meaningful) when Injection == InjectionQuery, mirroring
	// config.ServiceAuthConfig.Query.
	Query string
	// Username is the fixed Basic-auth username half — meaningful only when
	// Injection == InjectionBasic, and mutually exclusive with
	// UsernameFrom (exactly one of the two is required for a basic slot).
	// Declared on the SLOT, not the instance: RFC 7617 usernames for a
	// token-as-password convention (e.g. Bitbucket's
	// "x-bitbucket-api-token-auth") are a property of the API itself, which
	// the Pack author knows and an operator should never have to supply per
	// instance — "仕様は profile、値は instance" (signal-driven-review.md
	// §7.1) applied to this one field.
	Username string
	// UsernameFrom selects the Basic-auth username SOURCE when Injection ==
	// InjectionBasic: the zero value ("") means Username above supplies a
	// Pack-fixed value (the Bitbucket case); the only other recognized
	// value, UsernameFromInstance ("instance"), means the username is
	// INSTANCE-SPECIFIC — the Pack only declares THAT an instance must
	// supply one, not the value itself (still "仕様は profile、値は
	// instance", just applied the other way around from Username: here the
	// profile's "仕様" is merely "an instance-supplied username is
	// required"). This is what Jira Cloud's Basic-auth convention needs
	// (username = the operator's own Atlassian account email, which differs
	// per instance and so cannot be a fixed manifest constant the way
	// Bitbucket's convention is). Mutually exclusive with Username; DesugarService
	// is where the instance's actual value (config.ServiceConfig.Username)
	// gets bound in. Meaningless (and rejected by parseCredentialSlot) for
	// any injection other than basic.
	UsernameFrom string
}

// UsernameFromInstance is the only value CredentialSlot.UsernameFrom
// currently recognizes — see that field's own doc comment.
const UsernameFromInstance = "instance"

// Connector is one connectors[] entry.
type Connector struct {
	Name           string
	Executable     string
	ServiceProfile string
	// ConfigSchema validates a connector's declared config (§6.2 item 4).
	// nil means the connector takes no config at all.
	ConfigSchema *ConfigSchema
}

// Skill is one skills[] entry.
type Skill struct {
	Name string
	Path string
	// RequiresServiceProfile, when non-empty, must name a ServiceProfiles[]
	// entry declared in the same manifest — mirrors Connector.ServiceProfile.
	RequiresServiceProfile string
}

// rawManifest/raw* mirror Manifest/*'s shape field-for-field for strict
// YAML decoding only — see ParseManifest's own doc comment for why a
// separate decode-only type is used rather than adding yaml tags/
// CredentialInjection directly to the public types.
type rawManifest struct {
	APIVersion      string              `yaml:"apiVersion"`
	Kind            string              `yaml:"kind"`
	Metadata        rawMetadata         `yaml:"metadata"`
	ServiceProfiles []rawServiceProfile `yaml:"serviceProfiles,omitempty"`
	Connectors      []rawConnector      `yaml:"connectors,omitempty"`
	Skills          []rawSkill          `yaml:"skills,omitempty"`
}

type rawMetadata struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

type rawServiceProfile struct {
	Name        string              `yaml:"name"`
	Endpoint    rawEndpoint         `yaml:"endpoint,omitempty"`
	Credentials []rawCredentialSlot `yaml:"credentials,omitempty"`
}

type rawEndpoint struct {
	Configurable bool `yaml:"configurable"`
}

type rawCredentialSlot struct {
	Name         string `yaml:"name"`
	Injection    string `yaml:"injection"`
	Header       string `yaml:"header,omitempty"`
	Query        string `yaml:"query,omitempty"`
	Username     string `yaml:"username,omitempty"`
	UsernameFrom string `yaml:"usernameFrom,omitempty"`
}

type rawConnector struct {
	Name           string           `yaml:"name"`
	Executable     string           `yaml:"executable"`
	ServiceProfile string           `yaml:"serviceProfile"`
	ConfigSchema   *rawConfigSchema `yaml:"configSchema,omitempty"`
}

type rawConfigSchema struct {
	Type       string                       `yaml:"type"`
	Properties map[string]rawPropertySchema `yaml:"properties,omitempty"`
	Required   []string                     `yaml:"required,omitempty"`
}

type rawPropertySchema struct {
	Type string `yaml:"type"`
}

type rawSkill struct {
	Name                   string `yaml:"name"`
	Path                   string `yaml:"path"`
	RequiresServiceProfile string `yaml:"requiresServiceProfile,omitempty"`
}

// ParseManifest strict-decodes and structurally validates one
// integration.yaml document (docs/plans/signal-driven-review.md §6.3):
//
//   - unknown fields at ANY depth are a hard error (yaml.Decoder.
//     KnownFields(true) — docs/plans/signal-ingest-detailed-design.md §6.2
//     item 1: "未知 field はエラー (strict decode。黙って無視しない)")
//   - apiVersion must equal ManifestAPIVersion, kind must equal ManifestKind
//   - metadata.name/version are required
//   - a service profile may declare at most one credential slot (§6.2's
//     "先頭 slot を勝手に取るような縮退をしない" — more than one is a hard
//     error, not a silently-take-the-first fallback)
//   - each credential slot's injection is one of bearer/basic/header/query,
//     with its injection-conditional field (header/query/username, or for
//     basic: exactly one of username/usernameFrom) required
//   - connectors[].serviceProfile and skills[].requiresServiceProfile must
//     each name a serviceProfiles[] entry declared in the SAME manifest
//   - a connector's configSchema, if present, is itself validated against
//     the v0 subset (root type must be "object", every property type one
//     of string/number/boolean, required[] only names declared properties)
//
// ParseManifest does NOT check Metadata against an installation directory
// — LoadPacks (pack.go) does that, since only it knows the directory a
// manifest was found under.
func ParseManifest(data []byte) (*Manifest, error) {
	var raw rawManifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("integrationpack: parse manifest: %w", err)
	}
	// F6, review finding (recommended): yaml.Decoder.Decode only ever reads
	// ONE "---"-separated document per call — a manifest accidentally
	// containing a second document (a stray paste, two manifests
	// concatenated) used to have that second document silently vanish
	// rather than fail loudly. A second Decode call into a throwaway value
	// returning anything other than io.EOF means more content follows.
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("integrationpack: parse manifest: file contains more than one YAML document")
		}
		return nil, fmt.Errorf("integrationpack: parse manifest: %w", err)
	}

	if raw.APIVersion != ManifestAPIVersion {
		return nil, fmt.Errorf("integrationpack: unsupported apiVersion %q (want %q)", raw.APIVersion, ManifestAPIVersion)
	}
	if raw.Kind != ManifestKind {
		return nil, fmt.Errorf("integrationpack: unsupported kind %q (want %q)", raw.Kind, ManifestKind)
	}
	if raw.Metadata.Name == "" {
		return nil, fmt.Errorf("integrationpack: metadata.name is required")
	}
	if raw.Metadata.Version == "" {
		return nil, fmt.Errorf("integrationpack: metadata.version is required")
	}

	m := &Manifest{
		APIVersion: raw.APIVersion,
		Kind:       raw.Kind,
		Metadata:   ManifestMetadata{Name: raw.Metadata.Name, Version: raw.Metadata.Version},
	}

	profileNames := make(map[string]bool, len(raw.ServiceProfiles))
	for _, rsp := range raw.ServiceProfiles {
		if rsp.Name == "" {
			return nil, fmt.Errorf("integrationpack: serviceProfiles: a profile name must not be empty")
		}
		// F4, review finding (MEDIUM): a duplicate profile name used to
		// parse cleanly, and Pack.ServiceProfile's lookup silently returned
		// whichever entry came first in the slice — the same "先頭 slot を
		// 勝手に取るような縮退をしない" v0 restriction the credential-slot
		// count check just below already enforces, one level up (profile
		// name instead of slot name). No silent first-match degradation.
		if profileNames[rsp.Name] {
			return nil, fmt.Errorf("integrationpack: serviceProfiles: duplicate profile name %q", rsp.Name)
		}
		if len(rsp.Credentials) > 1 {
			return nil, fmt.Errorf("integrationpack: serviceProfiles[%q]: declares %d credential slots, v0 supports at most 1 (no silent take-the-first fallback)", rsp.Name, len(rsp.Credentials))
		}
		profile := ServiceProfile{
			Name:     rsp.Name,
			Endpoint: EndpointSpec{Configurable: rsp.Endpoint.Configurable},
		}
		for _, rc := range rsp.Credentials {
			slot, err := parseCredentialSlot(rsp.Name, rc)
			if err != nil {
				return nil, err
			}
			profile.Credentials = append(profile.Credentials, slot)
		}
		profileNames[rsp.Name] = true
		m.ServiceProfiles = append(m.ServiceProfiles, profile)
	}

	connectorNames := make(map[string]bool, len(raw.Connectors))
	for _, rc := range raw.Connectors {
		if rc.Name == "" {
			return nil, fmt.Errorf("integrationpack: connectors: a connector name must not be empty")
		}
		// F4 (MEDIUM) — same no-silent-first-match posture as
		// serviceProfiles above.
		if connectorNames[rc.Name] {
			return nil, fmt.Errorf("integrationpack: connectors: duplicate connector name %q", rc.Name)
		}
		connectorNames[rc.Name] = true
		// F5, review finding (recommended): PR-5 resolves executable
		// relative to this Pack version's own directory (docs/plans/
		// signal-ingest-detailed-design.md §7.1 "mount 位置") and uses it
		// as an exec source — an empty, absolute, or "../"-escaping value
		// was accepted here with no check at all before this fix.
		// filepath.IsLocal (path/filepath) is exactly this guarantee:
		// non-empty, relative, and never escapes its own directory via
		// "..".
		if !filepath.IsLocal(rc.Executable) || !packPathPattern.MatchString(rc.Executable) {
			return nil, fmt.Errorf("integrationpack: connectors[%q]: executable %q must be a non-empty path local to the Pack directory (no absolute path, no \"..\") matching %s per path segment", rc.Name, rc.Executable, packPathPattern.String())
		}
		if rc.ServiceProfile == "" {
			return nil, fmt.Errorf("integrationpack: connectors[%q]: serviceProfile is required", rc.Name)
		}
		if !profileNames[rc.ServiceProfile] {
			return nil, fmt.Errorf("integrationpack: connectors[%q]: serviceProfile %q is not declared under serviceProfiles", rc.Name, rc.ServiceProfile)
		}
		var schema *ConfigSchema
		if rc.ConfigSchema != nil {
			s, err := parseConfigSchema(rc.Name, rc.ConfigSchema)
			if err != nil {
				return nil, err
			}
			schema = s
		}
		m.Connectors = append(m.Connectors, Connector{
			Name:           rc.Name,
			Executable:     rc.Executable,
			ServiceProfile: rc.ServiceProfile,
			ConfigSchema:   schema,
		})
	}

	skillNames := make(map[string]bool, len(raw.Skills))
	for _, rs := range raw.Skills {
		if rs.Name == "" {
			return nil, fmt.Errorf("integrationpack: skills: a skill name must not be empty")
		}
		// skillNamePattern, review finding (BLOCKER, Opus round 2): name is
		// used downstream as a symlink basename joined straight into
		// <discovery root>/<name> (internal/dispatcher's skillLinks /
		// SkillLink) — a DENYLIST there (reject "/", ".", "..", ...)
		// kept growing a new hole per review round, most recently a NUL
		// byte: filepath.IsLocal("\x00") is true and filepath.Base("\x00")
		// == "\x00", so it survived every denylist check, and bash strips
		// NUL from a heredoc-fed script before executing it — turning `rm
		// -rf -- '.claude/skills/<NUL>'` into `rm -rf -- '.claude/skills/'`
		// (confirmed against a real shell), the same "wipe every embedded
		// skill's bind target" failure the "/" case had, except this one
		// exits non-zero and never writes a completion marker, so it
		// repeats on every subsequent dispatch rather than settling.
		// An ALLOWLIST closes the whole denylist-of-the-week pattern at
		// once: this is validated once, at manifest load (a malformed
		// manifest already fails daemon startup outright — see LoadPacks'
		// own "検証失敗は起動エラー" posture), and internal/dispatcher's
		// skillLinks keeps its own equivalent filter as defense in
		// depth for any SkillLink built some other way (tests, a future
		// constructor) rather than trusting this one gate transitively.
		if !skillNamePattern.MatchString(rs.Name) {
			return nil, fmt.Errorf("integrationpack: skills[%q]: name must match %s (used as a symlink basename under .claude/skills/)", rs.Name, skillNamePattern.String())
		}
		// F4 (MEDIUM) — same no-silent-first-match posture as
		// serviceProfiles/connectors above.
		if skillNames[rs.Name] {
			return nil, fmt.Errorf("integrationpack: skills: duplicate skill name %q", rs.Name)
		}
		skillNames[rs.Name] = true
		// F5 (recommended) — same non-empty/local-path guarantee as
		// connectors[].executable above; PR-5's selective skill mount
		// resolves this relative to the Pack's own directory too.
		if !filepath.IsLocal(rs.Path) || !packPathPattern.MatchString(rs.Path) {
			return nil, fmt.Errorf("integrationpack: skills[%q]: path %q must be a non-empty path local to the Pack directory (no absolute path, no \"..\") matching %s per path segment", rs.Name, rs.Path, packPathPattern.String())
		}
		if rs.RequiresServiceProfile != "" && !profileNames[rs.RequiresServiceProfile] {
			return nil, fmt.Errorf("integrationpack: skills[%q]: requiresServiceProfile %q is not declared under serviceProfiles", rs.Name, rs.RequiresServiceProfile)
		}
		m.Skills = append(m.Skills, Skill(rs))
	}

	return m, nil
}

// parseCredentialSlot validates and converts one raw credential slot,
// enforcing the injection-conditional required field
// (docs/plans/signal-ingest-detailed-design.md §6.2's "injection:
// bearer|basic|header|query + header 名").
func parseCredentialSlot(profileName string, rc rawCredentialSlot) (CredentialSlot, error) {
	if rc.Name == "" {
		return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q]: credentials: a slot name must not be empty", profileName)
	}
	// usernameFrom is only ever meaningful for injection: basic — checked
	// once, up front, so every other injection arm below doesn't need its
	// own copy of this guard (mirrors how the switch below already leaves
	// "username" unchecked for non-basic injections, but a NEW field gets
	// no such legacy leniency).
	if rc.UsernameFrom != "" && CredentialInjection(rc.Injection) != InjectionBasic {
		return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q].credentials[%q]: \"usernameFrom\" is only meaningful for injection basic (got injection %q)", profileName, rc.Name, rc.Injection)
	}
	switch CredentialInjection(rc.Injection) {
	case InjectionBearer:
	case InjectionBasic:
		// Exactly one of "username" (a Pack-fixed value, e.g. Bitbucket's
		// "x-bitbucket-api-token-auth") or "usernameFrom: instance" (the
		// service instance supplies it, e.g. Jira Cloud's per-tenant
		// Atlassian account email) is required — "仕様は profile、値は
		// instance" (signal-driven-review.md §7.1) applied to this field
		// either way, just with the value living in a different place.
		switch {
		case rc.Username != "" && rc.UsernameFrom != "":
			return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q].credentials[%q]: injection basic must declare exactly one of \"username\" (a Pack-fixed Basic-auth username) or \"usernameFrom: instance\" (the service instance supplies it) — not both", profileName, rc.Name)
		case rc.UsernameFrom != "":
			if rc.UsernameFrom != UsernameFromInstance {
				return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q].credentials[%q]: usernameFrom: unrecognized %q (want %q)", profileName, rc.Name, rc.UsernameFrom, UsernameFromInstance)
			}
		case rc.Username == "":
			return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q].credentials[%q]: injection basic requires either \"username\" (a Pack-fixed Basic-auth username) or \"usernameFrom: instance\" (the service instance supplies it)", profileName, rc.Name)
		}
	case InjectionHeader:
		if rc.Header == "" {
			return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q].credentials[%q]: injection header requires \"header\" (the header name to set)", profileName, rc.Name)
		}
	case InjectionQuery:
		if rc.Query == "" {
			return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q].credentials[%q]: injection query requires \"query\" (the query parameter name to set)", profileName, rc.Name)
		}
	default:
		return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q].credentials[%q]: injection: unrecognized %q (want one of bearer, basic, header, query)", profileName, rc.Name, rc.Injection)
	}
	return CredentialSlot{
		Name:         rc.Name,
		Injection:    CredentialInjection(rc.Injection),
		Header:       rc.Header,
		Query:        rc.Query,
		Username:     rc.Username,
		UsernameFrom: rc.UsernameFrom,
	}, nil
}

// parseConfigSchema validates and converts one raw connector configSchema
// against the v0 minimal JSON-Schema subset (docs/plans/
// signal-ingest-detailed-design.md §6.2 item 4: "type/object/properties/
// required、値型は string/number/boolean のみ").
func parseConfigSchema(connectorName string, raw *rawConfigSchema) (*ConfigSchema, error) {
	if raw.Type != "object" {
		return nil, fmt.Errorf("integrationpack: connectors[%q].configSchema.type must be \"object\" (v0 supports only an object schema at the root), got %q", connectorName, raw.Type)
	}
	schema := &ConfigSchema{Type: raw.Type, Required: append([]string(nil), raw.Required...)}
	if len(raw.Properties) > 0 {
		schema.Properties = make(map[string]PropertySchema, len(raw.Properties))
		for propName, prop := range raw.Properties {
			switch prop.Type {
			case "string", "number", "boolean":
			default:
				return nil, fmt.Errorf("integrationpack: connectors[%q].configSchema.properties[%q].type: unsupported %q (v0 supports string/number/boolean only)", connectorName, propName, prop.Type)
			}
			schema.Properties[propName] = PropertySchema(prop)
		}
	}
	for _, req := range schema.Required {
		if _, ok := schema.Properties[req]; !ok {
			return nil, fmt.Errorf("integrationpack: connectors[%q].configSchema.required references undeclared property %q", connectorName, req)
		}
	}
	return schema, nil
}
