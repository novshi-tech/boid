package integrationpack

import (
	"strings"
	"testing"
)

const validManifestYAML = `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata:
  name: jira-cloud
  version: 1.2.0
serviceProfiles:
  - name: jira-cloud
    endpoint:
      configurable: true
    credentials:
      - name: token
        injection: bearer
connectors:
  - name: assigned-issues
    executable: connectors/assigned-issues
    serviceProfile: jira-cloud
    configSchema:
      type: object
      properties:
        jql: {type: string}
      required: [jql]
skills:
  - name: jira-api
    path: skills/jira-api
    requiresServiceProfile: jira-cloud
`

// TestParseManifest_Valid pins the manifest shape docs/plans/
// signal-driven-review.md §6.3 gives as the canonical example — every field
// must round-trip into the typed Manifest struct.
func TestParseManifest_Valid(t *testing.T) {
	m, err := ParseManifest([]byte(validManifestYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.APIVersion != "boid.dev/v1" {
		t.Errorf("APIVersion = %q", m.APIVersion)
	}
	if m.Kind != "IntegrationPack" {
		t.Errorf("Kind = %q", m.Kind)
	}
	if m.Metadata.Name != "jira-cloud" || m.Metadata.Version != "1.2.0" {
		t.Errorf("Metadata = %+v", m.Metadata)
	}
	if len(m.ServiceProfiles) != 1 {
		t.Fatalf("len(ServiceProfiles) = %d, want 1", len(m.ServiceProfiles))
	}
	sp := m.ServiceProfiles[0]
	if sp.Name != "jira-cloud" {
		t.Errorf("ServiceProfiles[0].Name = %q", sp.Name)
	}
	if !sp.Endpoint.Configurable {
		t.Error("ServiceProfiles[0].Endpoint.Configurable = false, want true")
	}
	if len(sp.Credentials) != 1 || sp.Credentials[0].Name != "token" || sp.Credentials[0].Injection != InjectionBearer {
		t.Errorf("ServiceProfiles[0].Credentials = %+v", sp.Credentials)
	}
	if len(m.Connectors) != 1 {
		t.Fatalf("len(Connectors) = %d, want 1", len(m.Connectors))
	}
	c := m.Connectors[0]
	if c.Name != "assigned-issues" || c.Executable != "connectors/assigned-issues" || c.ServiceProfile != "jira-cloud" {
		t.Errorf("Connectors[0] = %+v", c)
	}
	if c.ConfigSchema == nil {
		t.Fatal("Connectors[0].ConfigSchema is nil")
	}
	if c.ConfigSchema.Type != "object" {
		t.Errorf("ConfigSchema.Type = %q", c.ConfigSchema.Type)
	}
	if c.ConfigSchema.Properties["jql"].Type != "string" {
		t.Errorf("ConfigSchema.Properties[jql].Type = %q", c.ConfigSchema.Properties["jql"].Type)
	}
	if len(c.ConfigSchema.Required) != 1 || c.ConfigSchema.Required[0] != "jql" {
		t.Errorf("ConfigSchema.Required = %v", c.ConfigSchema.Required)
	}
	if len(m.Skills) != 1 {
		t.Fatalf("len(Skills) = %d, want 1", len(m.Skills))
	}
	sk := m.Skills[0]
	if sk.Name != "jira-api" || sk.Path != "skills/jira-api" || sk.RequiresServiceProfile != "jira-cloud" {
		t.Errorf("Skills[0] = %+v", sk)
	}
}

// TestParseManifest_UnknownTopLevelFieldRejected pins Q19/§7.2's "未知
// field はエラー" strict-decode requirement (docs/plans/
// signal-ingest-detailed-design.md §6.2 item 1) — a typo'd or
// not-yet-supported top-level key must fail loudly, not be silently
// dropped.
func TestParseManifest_UnknownTopLevelFieldRejected(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata:
  name: jira-cloud
  version: 1.2.0
unknownField: surprise
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for unknown top-level field, got nil")
	}
	if !strings.Contains(err.Error(), "unknownField") && !strings.Contains(strings.ToLower(err.Error()), "not found") && !strings.Contains(strings.ToLower(err.Error()), "field") {
		t.Errorf("error should indicate an unknown/unrecognized field, got: %v", err)
	}
}

// TestParseManifest_UnknownNestedFieldRejected is the nested-struct
// counterpart — strict decode must cover serviceProfiles[]/connectors[]/
// skills[] entries too, not just the top level.
func TestParseManifest_UnknownNestedFieldRejected(t *testing.T) {
	cases := map[string]string{
		"metadata": `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata:
  name: jira-cloud
  version: 1.2.0
  extra: surprise
`,
		"serviceProfiles": `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: jira-cloud, version: 1.2.0}
serviceProfiles:
  - name: jira-cloud
    extra: surprise
`,
		"credentials": `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: jira-cloud, version: 1.2.0}
serviceProfiles:
  - name: jira-cloud
    credentials:
      - name: token
        injection: bearer
        extra: surprise
`,
		"connectors": `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: jira-cloud, version: 1.2.0}
connectors:
  - name: c
    executable: connectors/c
    serviceProfile: jira-cloud
    extra: surprise
`,
		"configSchema": `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: jira-cloud, version: 1.2.0}
connectors:
  - name: c
    executable: connectors/c
    serviceProfile: jira-cloud
    configSchema:
      type: object
      extra: surprise
`,
		"skills": `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: jira-cloud, version: 1.2.0}
skills:
  - name: jira-api
    path: skills/jira-api
    extra: surprise
`,
	}
	for label, yaml := range cases {
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("%s: want error for unknown nested field, got nil", label)
		}
	}
}

// TestParseManifest_UnknownAPIVersionRejected pins §7.3's "前方互換を装わ
// ない" rule: apiVersion is fixed at "boid.dev/v1"; anything else — a typo,
// or a genuinely newer contract version this binary predates — is rejected
// outright rather than loaded best-effort.
func TestParseManifest_UnknownAPIVersionRejected(t *testing.T) {
	cases := []string{"boid.dev/v2", "v1", ""}
	for _, v := range cases {
		yaml := "apiVersion: " + v + "\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n"
		if v == "" {
			yaml = "kind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n"
		}
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("apiVersion %q: want error, got nil", v)
		}
	}
}

// TestParseManifest_WrongKindRejected pins that kind must be
// "IntegrationPack" — a structural sanity check on the manifest's own
// self-description.
func TestParseManifest_WrongKindRejected(t *testing.T) {
	yaml := "apiVersion: boid.dev/v1\nkind: SomethingElse\nmetadata: {name: x, version: '1.0.0'}\n"
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for wrong kind, got nil")
	}
}

// TestParseManifest_MissingMetadataRejected pins that name/version are
// required — LoadPacks cross-checks both against the installation
// directory (see pack_test.go), so a manifest missing either can never
// resolve.
func TestParseManifest_MissingMetadataRejected(t *testing.T) {
	cases := []string{
		"apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {version: '1.0.0'}\n",
		"apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x}\n",
	}
	for _, yaml := range cases {
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("yaml %q: want error for missing metadata field, got nil", yaml)
		}
	}
}

// TestParseManifest_MultipleCredentialSlotsRejected pins the v0 restriction
// (docs/plans/signal-ingest-detailed-design.md §6.2: "profile の credential
// slot が複数ある Pack ... は v0 では起動エラー" — Q19) — no
// silently-take-the-first-slot degradation.
func TestParseManifest_MultipleCredentialSlotsRejected(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials:
      - name: token
        injection: bearer
      - name: secondary
        injection: header
        header: X-Extra
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for multiple credential slots, got nil")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("error should mention credential slots, got: %v", err)
	}
}

// TestParseManifest_UnknownInjectionRejected pins that credential slot
// injection is restricted to bearer/basic/header/query — oauth2 (and any
// other value) is deliberately unsupported for a Pack-declared profile in
// v0 (signal-driven-review.md §7.1's OAuth2 profile support remains a §12
// open question).
func TestParseManifest_UnknownInjectionRejected(t *testing.T) {
	cases := []string{"oauth2", "hmac", ""}
	for _, injection := range cases {
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"serviceProfiles:\n  - name: x\n    credentials:\n      - name: token\n        injection: \"" + injection + "\"\n"
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("injection %q: want error, got nil", injection)
		}
	}
}

// TestParseManifest_HeaderInjectionRequiresHeaderName pins the required
// pairing (docs/plans/signal-ingest-detailed-design.md §6.2's "injection:
// bearer|basic|header|query + header 名").
func TestParseManifest_HeaderInjectionRequiresHeaderName(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials:
      - name: token
        injection: header
`
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for header injection with no header name, got nil")
	}
}

// TestParseManifest_QueryInjectionRequiresQueryName mirrors
// TestParseManifest_HeaderInjectionRequiresHeaderName for injection: query.
func TestParseManifest_QueryInjectionRequiresQueryName(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials:
      - name: token
        injection: query
`
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for query injection with no query param name, got nil")
	}
}

// TestParseManifest_BasicInjectionRequiresUsername mirrors
// TestParseManifest_HeaderInjectionRequiresHeaderName for injection: basic
// — the fixed username half of a Basic credential is a Pack-author-level
// constant (e.g. Bitbucket's "x-token-auth" convention), not something an
// operator supplies per instance, so it lives on the credential slot
// itself.
func TestParseManifest_BasicInjectionRequiresUsername(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials:
      - name: token
        injection: basic
`
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for basic injection with no username, got nil")
	}
}

// TestParseManifest_ConnectorUnknownServiceProfileRejected pins
// manifest-internal consistency: a connector's serviceProfile must name a
// serviceProfiles[] entry declared in the SAME manifest (there is nowhere
// else it could resolve from).
func TestParseManifest_ConnectorUnknownServiceProfileRejected(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials: [{name: token, injection: bearer}]
connectors:
  - name: c
    executable: connectors/c
    serviceProfile: nonexistent
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for connector referencing an unknown service profile, got nil")
	}
}

// TestParseManifest_SkillRequiresUnknownServiceProfileRejected mirrors
// TestParseManifest_ConnectorUnknownServiceProfileRejected for
// skills[].requiresServiceProfile.
func TestParseManifest_SkillRequiresUnknownServiceProfileRejected(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
skills:
  - name: s
    path: skills/s
    requiresServiceProfile: nonexistent
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for skill requiring an unknown service profile, got nil")
	}
}

// TestParseManifest_ConfigSchemaRootTypeMustBeObject pins the v0 subset's
// scope (docs/plans/signal-ingest-detailed-design.md §6.2 item 4: "type/
// object/properties/required") — a non-object root schema is not
// implemented in v0.
func TestParseManifest_ConfigSchemaRootTypeMustBeObject(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials: [{name: token, injection: bearer}]
connectors:
  - name: c
    executable: connectors/c
    serviceProfile: x
    configSchema:
      type: array
`
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for a non-object root configSchema, got nil")
	}
}

// TestParseManifest_ConfigSchemaPropertyTypeMustBeSupported pins the v0
// subset's leaf types (string/number/boolean only).
func TestParseManifest_ConfigSchemaPropertyTypeMustBeSupported(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials: [{name: token, injection: bearer}]
connectors:
  - name: c
    executable: connectors/c
    serviceProfile: x
    configSchema:
      type: object
      properties:
        nested: {type: object}
`
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for an unsupported property schema type, got nil")
	}
}

// TestParseManifest_ConfigSchemaRequiredMustReferenceDeclaredProperty pins
// that required[] cannot name an undeclared property — likely a typo.
func TestParseManifest_ConfigSchemaRequiredMustReferenceDeclaredProperty(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials: [{name: token, injection: bearer}]
connectors:
  - name: c
    executable: connectors/c
    serviceProfile: x
    configSchema:
      type: object
      properties:
        jql: {type: string}
      required: [nonexistent]
`
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for required naming an undeclared property, got nil")
	}
}
