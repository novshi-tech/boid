package orchestrator

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog redirects the default slog logger to an in-memory buffer for
// the duration of the test. Mirrors orchestrator_test's own captureSlog
// (spec_loader_test.go) — duplicated here rather than shared because that
// helper lives in the external orchestrator_test package and is not
// reachable from this (internal, package orchestrator) test file.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

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
}

// TestDecodeWorkspaceMetaStrict_AdditionalBindingsToleratedAndIgnored pins
// the Phase 4 PR4 (docs/plans/home-workspace-volume.md) regression contract:
// an existing workspace yaml/PUT body that still declares a well-formed
// additional_bindings key must keep decoding without error (no "unknown
// field" rejection, unlike `kits:` — see workspaceMetaStrict's doc comment
// for why the two retired fields are handled differently), a warning must be
// logged so an operator notices the value is silently discarded, and the
// resulting WorkspaceMeta must carry none of it (the type has no field to
// carry it in any more).
func TestDecodeWorkspaceMetaStrict_AdditionalBindingsToleratedAndIgnored(t *testing.T) {
	buf := captureSlog(t)
	data := []byte(`
env:
  FOO: bar
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
		t.Errorf("Env[FOO] = %q, want bar (rest of the document must still decode)", meta.Env["FOO"])
	}
	if !strings.Contains(buf.String(), "additional_bindings") {
		t.Errorf("expected a warning mentioning additional_bindings, got log: %s", buf.String())
	}
}

// TestDecodeWorkspaceMetaStrict_MalformedAdditionalBindingsToleratedNoWarning
// pins the other half of the tolerate-and-ignore contract: since the value
// is discarded either way, even a shape BindMount could never have accepted
// (a nested typo, or a completely different type) must not error — there is
// nothing left downstream that would validate its structure. An absent key
// must not warn (the common, post-Phase-4 case).
func TestDecodeWorkspaceMetaStrict_MalformedAdditionalBindingsToleratedNoWarning(t *testing.T) {
	buf := captureSlog(t)
	data := []byte(`
additional_bindings:
  - source: /opt/volta
    modee: rw
`)
	if _, err := DecodeWorkspaceMetaStrict(data); err != nil {
		t.Fatalf("DecodeWorkspaceMetaStrict: %v (a malformed additional_bindings shape must not error — the value is discarded either way)", err)
	}
	if !strings.Contains(buf.String(), "additional_bindings") {
		t.Errorf("expected a warning mentioning additional_bindings, got log: %s", buf.String())
	}

	buf2 := captureSlog(t)
	if _, err := DecodeWorkspaceMetaStrict([]byte("env:\n  FOO: bar\n")); err != nil {
		t.Fatalf("DecodeWorkspaceMetaStrict: %v", err)
	}
	if strings.Contains(buf2.String(), "additional_bindings") {
		t.Errorf("expected no additional_bindings warning when the key is absent, got log: %s", buf2.String())
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
