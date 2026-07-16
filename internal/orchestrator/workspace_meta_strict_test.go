package orchestrator

import (
	"strings"
	"testing"
)

// TestDecodeWorkspaceMetaStrict_RoundTrip verifies that every WorkspaceMeta
// field decodes correctly through the strict path.
func TestDecodeWorkspaceMetaStrict_RoundTrip(t *testing.T) {
	data := []byte(`
env:
  FOO: bar
capabilities:
  docker: {}
allowed_domains:
  - example.com
extra_repos:
  - https://github.com/example/lib.git
host_commands:
  - gh
  - aws
container_image: ghcr.io/example/image:latest
additional_bindings:
  - source: /opt/volta
    target: /opt/volta
    mode: rw
`)
	meta, err := DecodeWorkspaceMetaStrict(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceMetaStrict: %v", err)
	}
	if meta.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want bar", meta.Env["FOO"])
	}
	if meta.Capabilities.Docker == nil {
		t.Error("Capabilities.Docker: got nil, want non-nil")
	}
	if !equalStringSlice(meta.AllowedDomains, []string{"example.com"}) {
		t.Errorf("AllowedDomains = %v", meta.AllowedDomains)
	}
	if !equalStringSlice(meta.ExtraRepos, []string{"https://github.com/example/lib.git"}) {
		t.Errorf("ExtraRepos = %v", meta.ExtraRepos)
	}
	if !equalStringSlice(meta.HostCommands, []string{"gh", "aws"}) {
		t.Errorf("HostCommands = %v", meta.HostCommands)
	}
	if meta.ContainerImage != "ghcr.io/example/image:latest" {
		t.Errorf("ContainerImage = %q", meta.ContainerImage)
	}
	if len(meta.AdditionalBindings) != 1 {
		t.Fatalf("AdditionalBindings: got %v", meta.AdditionalBindings)
	}
	want := BindMount{Source: "/opt/volta", Target: "/opt/volta", Mode: "rw"}
	if meta.AdditionalBindings[0] != want {
		t.Errorf("AdditionalBindings[0] = %+v, want %+v", meta.AdditionalBindings[0], want)
	}
}

// TestDecodeWorkspaceMetaStrict_EmptyBody verifies an empty document decodes
// to a zero-value WorkspaceMeta rather than erroring on io.EOF — an empty
// `boid workspace create <slug>` body (no --from-file) must succeed.
func TestDecodeWorkspaceMetaStrict_EmptyBody(t *testing.T) {
	meta, err := DecodeWorkspaceMetaStrict(nil)
	if err != nil {
		t.Fatalf("DecodeWorkspaceMetaStrict(nil): %v", err)
	}
	if meta == nil {
		t.Fatal("expected non-nil empty WorkspaceMeta")
	}
	if len(meta.Env) != 0 || len(meta.HostCommands) != 0 {
		t.Errorf("expected zero-value meta, got %+v", meta)
	}
}

// TestDecodeWorkspaceMetaStrict_RejectsUnknownTopLevelField pins the strict
// decode contract: a typo'd top-level key must error, not be silently
// dropped.
func TestDecodeWorkspaceMetaStrict_RejectsUnknownTopLevelField(t *testing.T) {
	_, err := DecodeWorkspaceMetaStrict([]byte("hostcommands: [gh]\n")) // typo: missing underscore
	if err == nil {
		t.Fatal("expected error for unknown top-level field, got nil")
	}
}

// TestDecodeWorkspaceMetaStrict_RejectsUnknownNestedBindMountField pins the
// "nested strict trap" (memory: strict yaml decode の nested trap — the same
// class of bug PR2's hostCommandSpecStrict exists to prevent): BindMount has
// its own custom UnmarshalYAML (for the short-form scalar convenience), and
// naively decoding through the public BindMount type would let a typo'd
// nested field silently vanish even under a KnownFields(true) top-level
// Decoder — because BindMount.UnmarshalYAML calls node.Decode(&aux)
// internally, always with default (non-strict) settings, regardless of the
// Decoder that produced the node.
func TestDecodeWorkspaceMetaStrict_RejectsUnknownNestedBindMountField(t *testing.T) {
	data := []byte(`
additional_bindings:
  - source: /opt/volta
    modee: rw
`) // typo: "modee" instead of "mode"
	_, err := DecodeWorkspaceMetaStrict(data)
	if err == nil {
		t.Fatal("expected error for unknown nested additional_bindings field, got nil")
	}
	if !strings.Contains(err.Error(), "modee") {
		t.Errorf("expected error to mention the typo'd field, got: %v", err)
	}
}

// TestDecodeWorkspaceMetaStrict_AcceptsScalarBindMountShortForm pins MINOR
// 3-a (codex review): the public yaml-authoring contract for
// additional_bindings allows a scalar short form (`additional_bindings:
// [/path]`, equivalent to `{source: /path}` — see BindMount.UnmarshalYAML's
// doc comment). Before this fix, bindMountStrict had no custom
// UnmarshalYAML at all (deliberately, to keep the nested-strict guarantee
// the test above pins) and so only accepted the full mapping form,
// rejecting the scalar short form workspace yaml documents are otherwise
// allowed to use.
func TestDecodeWorkspaceMetaStrict_AcceptsScalarBindMountShortForm(t *testing.T) {
	data := []byte(`
additional_bindings:
  - /opt/volta
`)
	meta, err := DecodeWorkspaceMetaStrict(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceMetaStrict: %v", err)
	}
	if len(meta.AdditionalBindings) != 1 {
		t.Fatalf("AdditionalBindings: got %v", meta.AdditionalBindings)
	}
	want := BindMount{Source: "/opt/volta"}
	if meta.AdditionalBindings[0] != want {
		t.Errorf("AdditionalBindings[0] = %+v, want %+v", meta.AdditionalBindings[0], want)
	}
}

// TestDecodeWorkspaceMetaStrict_AcceptsMixedScalarAndMappingBindMounts
// verifies the scalar short form and the full mapping form can coexist in
// the same additional_bindings list.
func TestDecodeWorkspaceMetaStrict_AcceptsMixedScalarAndMappingBindMounts(t *testing.T) {
	data := []byte(`
additional_bindings:
  - /opt/volta
  - source: /opt/tool
    target: /opt/tool
    mode: rw
`)
	meta, err := DecodeWorkspaceMetaStrict(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceMetaStrict: %v", err)
	}
	if len(meta.AdditionalBindings) != 2 {
		t.Fatalf("AdditionalBindings: got %v", meta.AdditionalBindings)
	}
	if meta.AdditionalBindings[0] != (BindMount{Source: "/opt/volta"}) {
		t.Errorf("AdditionalBindings[0] = %+v", meta.AdditionalBindings[0])
	}
	want1 := BindMount{Source: "/opt/tool", Target: "/opt/tool", Mode: "rw"}
	if meta.AdditionalBindings[1] != want1 {
		t.Errorf("AdditionalBindings[1] = %+v, want %+v", meta.AdditionalBindings[1], want1)
	}
}

// TestDecodeWorkspaceMetaStrict_RejectsUnknownNestedCapabilitiesField pins
// the same nested-strict guarantee for capabilities.docker (a typo there
// should not be silently ignored either, even though Capabilities itself has
// no custom UnmarshalYAML).
func TestDecodeWorkspaceMetaStrict_RejectsUnknownNestedCapabilitiesField(t *testing.T) {
	data := []byte(`
capabilities:
  dockerr: {}
`)
	_, err := DecodeWorkspaceMetaStrict(data)
	if err == nil {
		t.Fatal("expected error for unknown nested capabilities field, got nil")
	}
}

// TestStrictDecoder_RejectsMultipleYAMLDocuments pins MINOR 2 (codex review
// round 2, docs/plans/workspace-db-consolidation.md): yaml.Decoder.Decode
// only ever consumes the first "---"-delimited document and silently ignores
// anything after it. Before this fix, a hand-authored yaml with two
// documents would have its second document silently dropped — the reported
// meta would only ever reflect the first one, with no error at all
// indicating data loss.
func TestStrictDecoder_RejectsMultipleYAMLDocuments(t *testing.T) {
	data := []byte(`
env:
  FOO: bar
---
env:
  BAR: baz
`)
	_, err := DecodeWorkspaceMetaStrict(data)
	if err == nil {
		t.Fatal("expected error for a multi-document yaml body, got nil")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("expected error to mention multiple documents, got: %v", err)
	}
}

// TestDecodeWorkspaceCreateStrict_RejectsMultipleYAMLDocuments is the
// DecodeWorkspaceCreateStrict counterpart of the test above — the same
// silent-data-loss bug applies to the POST /api/workspaces create body.
func TestDecodeWorkspaceCreateStrict_RejectsMultipleYAMLDocuments(t *testing.T) {
	data := []byte(`
slug: team-a
env:
  FOO: bar
---
slug: team-b
env:
  BAR: baz
`)
	_, _, err := DecodeWorkspaceCreateStrict(data)
	if err == nil {
		t.Fatal("expected error for a multi-document yaml body, got nil")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("expected error to mention multiple documents, got: %v", err)
	}
}

// --- DecodeWorkspaceCreateStrict (POST /api/workspaces body: slug + meta) ---

func TestDecodeWorkspaceCreateStrict_ExtractsSlugAndMeta(t *testing.T) {
	data := []byte(`
slug: team-a
host_commands:
  - gh
env:
  FOO: bar
`)
	slug, meta, err := DecodeWorkspaceCreateStrict(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceCreateStrict: %v", err)
	}
	if slug != "team-a" {
		t.Errorf("slug = %q, want team-a", slug)
	}
	if !equalStringSlice(meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v", meta.HostCommands)
	}
	if meta.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want bar", meta.Env["FOO"])
	}
}

func TestDecodeWorkspaceCreateStrict_EmptyBody(t *testing.T) {
	slug, meta, err := DecodeWorkspaceCreateStrict(nil)
	if err != nil {
		t.Fatalf("DecodeWorkspaceCreateStrict(nil): %v", err)
	}
	if slug != "" {
		t.Errorf("slug = %q, want empty", slug)
	}
	if meta == nil {
		t.Fatal("expected non-nil empty meta")
	}
}

func TestDecodeWorkspaceCreateStrict_RejectsUnknownField(t *testing.T) {
	_, _, err := DecodeWorkspaceCreateStrict([]byte("slug: team-a\nhostcommands: [gh]\n"))
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}
