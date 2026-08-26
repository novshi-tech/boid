package integrationpack

import (
	"bytes"
	"fmt"

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
	// Username is the fixed Basic-auth username half — required (and only
	// meaningful) when Injection == InjectionBasic. Declared on the SLOT,
	// not the instance: RFC 7617 usernames for a token-as-password
	// convention (e.g. Bitbucket's "x-bitbucket-api-token-auth") are a
	// property of the API itself, which the Pack author knows and an
	// operator should never have to supply per instance — "仕様は profile、
	// 値は instance" (signal-driven-review.md §7.1) applied to this one
	// field.
	Username string
}

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
	Name      string `yaml:"name"`
	Injection string `yaml:"injection"`
	Header    string `yaml:"header,omitempty"`
	Query     string `yaml:"query,omitempty"`
	Username  string `yaml:"username,omitempty"`
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
//     with its injection-conditional field (header/query/username) required
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

	for _, rc := range raw.Connectors {
		if rc.Name == "" {
			return nil, fmt.Errorf("integrationpack: connectors: a connector name must not be empty")
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

	for _, rs := range raw.Skills {
		if rs.Name == "" {
			return nil, fmt.Errorf("integrationpack: skills: a skill name must not be empty")
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
	switch CredentialInjection(rc.Injection) {
	case InjectionBearer:
	case InjectionBasic:
		if rc.Username == "" {
			return CredentialSlot{}, fmt.Errorf("integrationpack: serviceProfiles[%q].credentials[%q]: injection basic requires \"username\" (the fixed Basic-auth username half)", profileName, rc.Name)
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
		Name:      rc.Name,
		Injection: CredentialInjection(rc.Injection),
		Header:    rc.Header,
		Query:     rc.Query,
		Username:  rc.Username,
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
