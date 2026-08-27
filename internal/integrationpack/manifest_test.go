package integrationpack

import (
	"fmt"
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

// TestParseManifest_BasicInjectionUsernameFromInstance pins the extension
// that lets a profile declare "usernameFrom: instance" instead of a fixed
// "username" — Jira Cloud's Basic-auth convention (username = the operator's
// Atlassian account email, password = API token) needs a DIFFERENT username
// per service instance, which the fixed-username field (Bitbucket's
// "x-bitbucket-api-token-auth" convention) cannot express (docs/plans/
// signal-driven-review.md §7.1's "仕様は profile、値は instance" applied to
// this one field: the profile only declares THAT the instance must supply a
// username, not the value itself).
func TestParseManifest_BasicInjectionUsernameFromInstance(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: jira-cloud, version: '1.0.0'}
serviceProfiles:
  - name: jira-cloud
    endpoint: {configurable: true}
    credentials:
      - name: token
        injection: basic
        usernameFrom: instance
`
	m, err := ParseManifest([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.ServiceProfiles) != 1 || len(m.ServiceProfiles[0].Credentials) != 1 {
		t.Fatalf("ServiceProfiles = %+v", m.ServiceProfiles)
	}
	slot := m.ServiceProfiles[0].Credentials[0]
	if slot.Injection != InjectionBasic {
		t.Errorf("Injection = %q, want basic", slot.Injection)
	}
	if slot.UsernameFrom != UsernameFromInstance {
		t.Errorf("UsernameFrom = %q, want %q", slot.UsernameFrom, UsernameFromInstance)
	}
	if slot.Username != "" {
		t.Errorf("Username = %q, want empty (usernameFrom: instance declares no fixed value)", slot.Username)
	}
}

// TestParseManifest_BasicInjectionUsernameAndUsernameFromMutuallyExclusive
// pins that a profile cannot declare both a fixed "username" AND
// "usernameFrom: instance" for the same slot — the two are contradictory
// (one says the value is fixed by the Pack, the other says the instance
// supplies it).
func TestParseManifest_BasicInjectionUsernameAndUsernameFromMutuallyExclusive(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials:
      - name: token
        injection: basic
        username: fixed-user
        usernameFrom: instance
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for both username and usernameFrom set, got nil")
	}
}

// TestParseManifest_UsernameFromUnrecognizedValueRejected pins that
// usernameFrom only recognizes "instance" today — any other value (a typo,
// or a not-yet-supported future source) is a hard error, not a silent
// no-op, matching the injection field's own "unrecognized %q" posture.
func TestParseManifest_UsernameFromUnrecognizedValueRejected(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials:
      - name: token
        injection: basic
        usernameFrom: workspace
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for unrecognized usernameFrom value, got nil")
	}
	if !strings.Contains(err.Error(), "usernameFrom") {
		t.Errorf("error should mention usernameFrom, got: %v", err)
	}
}

// TestParseManifest_UsernameFromOnNonBasicInjectionRejected pins that
// usernameFrom is only meaningful for injection: basic — declaring it
// alongside bearer/header/query is a likely copy-paste mistake that should
// fail loudly rather than being silently ignored (mirroring username's own
// injection-conditional meaning).
func TestParseManifest_UsernameFromOnNonBasicInjectionRejected(t *testing.T) {
	cases := []string{"bearer", "header", "query"}
	for _, injection := range cases {
		extra := ""
		switch injection {
		case "header":
			extra = "\n        header: X-Api-Key"
		case "query":
			extra = "\n        query: api_key"
		}
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"serviceProfiles:\n  - name: x\n    credentials:\n      - name: token\n        injection: " + injection +
			"\n        usernameFrom: instance" + extra + "\n"
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("injection %q: want error for usernameFrom on a non-basic slot, got nil", injection)
		}
	}
}

// TestParseManifest_BitbucketCloudFixedUsernameStillParses is a regression
// test pinning the exact serviceProfiles[].credentials[] shape the
// boid-api-skills bitbucket-cloud Pack's integration.yaml uses today (a
// profile-fixed Basic-auth username, RFC 7617's token-as-password
// convention) — this extension must not require that Pack to change at all.
func TestParseManifest_BitbucketCloudFixedUsernameStillParses(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata:
  name: bitbucket-cloud
  version: 1.0.0

serviceProfiles:
  - name: bitbucket-cloud
    endpoint:
      configurable: true
    credentials:
      - name: token
        injection: basic
        username: x-bitbucket-api-token-auth

connectors:
  - name: pr-comments
    executable: connectors/pr-comments
    serviceProfile: bitbucket-cloud
    configSchema:
      type: object
      properties:
        workspace:
          type: string
      required:
        - workspace

skills:
  - name: bitbucket-api
    path: skills/bitbucket-api
    requiresServiceProfile: bitbucket-cloud
`
	m, err := ParseManifest([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	slot := m.ServiceProfiles[0].Credentials[0]
	if slot.Username != "x-bitbucket-api-token-auth" {
		t.Errorf("Username = %q", slot.Username)
	}
	if slot.UsernameFrom != "" {
		t.Errorf("UsernameFrom = %q, want empty (fixed-username profile)", slot.UsernameFrom)
	}
}

// TestParseManifest_DuplicateServiceProfileNameRejected pins a review
// finding (F4, MEDIUM): before this fix, a manifest declaring two
// serviceProfiles[] entries with the same name parsed cleanly, and
// Pack.ServiceProfile's lookup silently returned whichever one came first
// in the slice — the exact "先頭 slot を勝手に取るような縮退をしない" v0
// restriction (docs/plans/signal-ingest-detailed-design.md §6.2), just one
// level up (profile name instead of credential slot). No silent
// first-match degradation: duplicate profile names must be a hard
// ParseManifest error.
func TestParseManifest_DuplicateServiceProfileNameRejected(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: dup
    credentials: [{name: token, injection: bearer}]
  - name: dup
    endpoint: {configurable: true}
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for duplicate service profile name, got nil")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error should name the duplicate, got: %v", err)
	}
}

// TestParseManifest_DuplicateConnectorNameRejected mirrors
// TestParseManifest_DuplicateServiceProfileNameRejected for connectors[].
func TestParseManifest_DuplicateConnectorNameRejected(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials: [{name: token, injection: bearer}]
connectors:
  - name: dup
    executable: connectors/a
    serviceProfile: x
  - name: dup
    executable: connectors/b
    serviceProfile: x
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for duplicate connector name, got nil")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error should name the duplicate, got: %v", err)
	}
}

// TestParseManifest_DuplicateSkillNameRejected mirrors
// TestParseManifest_DuplicateServiceProfileNameRejected for skills[].
func TestParseManifest_DuplicateSkillNameRejected(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
skills:
  - name: dup
    path: skills/a
  - name: dup
    path: skills/b
`
	_, err := ParseManifest([]byte(yaml))
	if err == nil {
		t.Fatal("want error for duplicate skill name, got nil")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error should name the duplicate, got: %v", err)
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

// TestParseManifest_ConnectorExecutableRequired pins F5 (recommended
// review finding): an empty connectors[].executable used to parse cleanly
// — PR-5 uses it as the exec/bind-mount source, where an empty value is
// never meaningful.
func TestParseManifest_ConnectorExecutableRequired(t *testing.T) {
	yaml := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata: {name: x, version: '1.0.0'}
serviceProfiles:
  - name: x
    credentials: [{name: token, injection: bearer}]
connectors:
  - name: c
    serviceProfile: x
`
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for missing connectors[].executable, got nil")
	}
}

// TestParseManifest_ConnectorExecutablePathTraversalRejected pins F5: an
// absolute path or a "../" path-traversal executable used to parse
// cleanly, even though PR-5 resolves it relative to the Pack's own version
// directory (docs/plans/signal-ingest-detailed-design.md §7.1 "mount
// 位置") — either would let a malicious/buggy Pack point outside its own
// installed directory.
func TestParseManifest_ConnectorExecutablePathTraversalRejected(t *testing.T) {
	cases := []string{"/etc/passwd", "../../../etc/passwd", "connectors/../../escape"}
	for _, exe := range cases {
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"serviceProfiles:\n  - name: x\n    credentials: [{name: token, injection: bearer}]\n" +
			"connectors:\n  - name: c\n    executable: \"" + exe + "\"\n    serviceProfile: x\n"
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("executable %q: want error for a non-local path, got nil", exe)
		}
	}
}

// TestParseManifest_SkillPathRequired mirrors
// TestParseManifest_ConnectorExecutableRequired for skills[].path.
func TestParseManifest_SkillPathRequired(t *testing.T) {
	yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\nskills:\n  - name: s\n"
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for missing skills[].path, got nil")
	}
}

// TestParseManifest_SkillPathTraversalRejected mirrors
// TestParseManifest_ConnectorExecutablePathTraversalRejected for
// skills[].path.
func TestParseManifest_SkillPathTraversalRejected(t *testing.T) {
	cases := []string{"/etc/passwd", "../../../etc/passwd"}
	for _, p := range cases {
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"skills:\n  - name: s\n    path: \"" + p + "\"\n"
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("path %q: want error for a non-local path, got nil", p)
		}
	}
}

// TestParseManifest_SkillNameMustMatchAllowlist is the BLOCKER fix (Opus
// review round 2) for a denylist that kept growing a new hole per round:
// first "/" (filepath.Base("/") == "/" survives a naive single-component
// check), then "." (filepath.IsLocal(".") is true), and finally a NUL byte
// — filepath.IsLocal("\x00") is true, filepath.Base("\x00") == "\x00", and
// bash strips NUL from a script it reads on stdin before executing it, so a
// skill named "\x00" would turn "rm -rf -- '.claude/skills/<NUL>'" into "rm
// -rf -- '.claude/skills/'" — the same "wipe every embedded skill's bind
// target" failure the "/" case had, confirmed against a real shell, except
// this one exits non-zero and never settles (no completion marker gets
// written, so it repeats on every subsequent dispatch).
//
// skillNamePattern is a positive allowlist specifically to stop enumerating
// denylist entries one review round at a time.
func TestParseManifest_SkillNameMustMatchAllowlist(t *testing.T) {
	rejected := []string{
		"", ".", "..", "/", "//", "a/b", "/a", "../escape", "./x",
		"\x00", "a\x00b", "a\nb", "a\tb", "a b", "-leading-dash",
		"~home", "a*b", "a$b",
	}
	for _, name := range rejected {
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"skills:\n  - name: \"" + escapeYAMLDoubleQuoted(name) + "\"\n    path: skills/x\n"
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("skill name %q (escaped %q): want a rejection, got nil", name, yaml)
		}
	}

	accepted := []string{"jira-api", "bitbucket-api", "slack-api", "a", "a.b", "a_b", "a-b", "a123"}
	for _, name := range accepted {
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"skills:\n  - name: " + name + "\n    path: skills/x\n"
		m, err := ParseManifest([]byte(yaml))
		if err != nil {
			t.Errorf("skill name %q: want acceptance, got error: %v", name, err)
			continue
		}
		if len(m.Skills) != 1 || m.Skills[0].Name != name {
			t.Errorf("skill name %q: got Skills = %+v", name, m.Skills)
		}
	}
}

// TestParseManifest_SkillPathMustMatchAllowlist is SF1 (Opus review round
// 3): skills[].path had filepath.IsLocal alone, the exact denylist-only
// posture skills[].name was moved off of after B3. filepath.IsLocal is
// textual — it only rejects a ".." PATH ELEMENT — and does not stop a NUL
// byte from turning several textually-"safe" segments into a real ".." once
// something downstream (bash reading a heredoc-fed script) strips the NUL:
// "a\x00../b" contains no ".." element as IsLocal sees it, but bash
// executes it as "a../b" -> still not "..", but repeated segments compound:
// ".\x00./.\x00./etc/passwd" survives IsLocal (no bare ".." anywhere) yet
// bash-strips to "..../etc/passwd" which most shells/tools normalize
// through repeated ".." style escapes depending on how many segments are
// chained — packPathPattern closes this the same way skillNamePattern
// closes B3, by allowlisting characters instead of denying specific
// sequences.
func TestParseManifest_SkillPathMustMatchAllowlist(t *testing.T) {
	rejected := []string{
		"\x00", "a\x00b", "skills/a\x00b", "a\nb",
		".\x00./.\x00./.\x00./.\x00./.\x00./etc/passwd",
	}
	for _, p := range rejected {
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"skills:\n  - name: s\n    path: \"" + escapeYAMLDoubleQuoted(p) + "\"\n"
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("skill path %q: want a rejection, got nil", p)
		}
	}

	accepted := []string{"skills/jira-api", "connectors/mentions", "a", "a.b/c-d/e_f"}
	for _, p := range accepted {
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"skills:\n  - name: s\n    path: " + p + "\n"
		if _, err := ParseManifest([]byte(yaml)); err != nil {
			t.Errorf("skill path %q: want acceptance, got error: %v", p, err)
		}
	}
}

// TestParseManifest_ConnectorExecutableMustMatchAllowlist mirrors
// TestParseManifest_SkillPathMustMatchAllowlist for connectors[].executable
// — the same shell-reachable field pair SF1 flagged together.
func TestParseManifest_ConnectorExecutableMustMatchAllowlist(t *testing.T) {
	rejected := []string{"\x00", "a\x00b", "connectors/a\x00b"}
	for _, p := range rejected {
		yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
			"serviceProfiles:\n  - name: sp\n    endpoint: {configurable: false}\n" +
			"connectors:\n  - name: c\n    executable: \"" + escapeYAMLDoubleQuoted(p) + "\"\n    serviceProfile: sp\n"
		if _, err := ParseManifest([]byte(yaml)); err == nil {
			t.Errorf("connector executable %q: want a rejection, got nil", p)
		}
	}

	yaml := "apiVersion: boid.dev/v1\nkind: IntegrationPack\nmetadata: {name: x, version: '1.0.0'}\n" +
		"serviceProfiles:\n  - name: sp\n    endpoint: {configurable: false}\n" +
		"connectors:\n  - name: c\n    executable: connectors/mentions\n    serviceProfile: sp\n"
	if _, err := ParseManifest([]byte(yaml)); err != nil {
		t.Errorf("connector executable %q: want acceptance, got error: %v", "connectors/mentions", err)
	}
}

// escapeYAMLDoubleQuoted renders s as the body of a YAML double-quoted
// scalar, using \xNN escapes for every byte outside printable ASCII (and
// backslash/quote themselves) — the minimal escaping needed to carry
// arbitrary bytes, including NUL, through a YAML double-quoted string
// exactly as TestParseManifest_SkillNameMustMatchAllowlist's rejected list
// intends them to reach ParseManifest.
func escapeYAMLDoubleQuoted(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}
	return b.String()
}

// TestParseManifest_MultipleYAMLDocumentsRejected pins F6 (recommended
// review finding): a "---"-separated second YAML document used to be
// silently ignored (yaml.Decoder.Decode only reads the first document) —
// an operator who accidentally pasted a stray second document (or
// concatenated two manifests) would have the second half silently vanish
// rather than fail loudly.
func TestParseManifest_MultipleYAMLDocumentsRejected(t *testing.T) {
	yaml := validManifestYAML + "\n---\nfoo: bar\n"
	if _, err := ParseManifest([]byte(yaml)); err == nil {
		t.Fatal("want error for a manifest containing more than one YAML document, got nil")
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
