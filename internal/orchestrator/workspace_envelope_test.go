package orchestrator

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// DecodeWorkspaceEnvelopeDocuments (docs/plans/volume-only-daemon.md §論点 g)
// ---------------------------------------------------------------------------

func TestDecodeWorkspaceEnvelopeDocuments_SingleDocument(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  host_commands:
    - atl
    - gh
  env:
    ATL_SITE: ubs
  allowed_domains: []
  capabilities:
    docker: {}
  projects:
    - name: rook-server
      url: git@bitbucket.org:Aolani-ondemand/rook-server.git
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("len(docs) = %d, want 1", len(docs))
	}
	doc := docs[0]
	if doc.Envelope.Metadata.Name != "default" {
		t.Errorf("Metadata.Name = %q, want default", doc.Envelope.Metadata.Name)
	}
	if !equalStrSlice(doc.Envelope.Spec.HostCommands, []string{"atl", "gh"}) {
		t.Errorf("HostCommands = %v", doc.Envelope.Spec.HostCommands)
	}
	if doc.Envelope.Spec.Env["ATL_SITE"] != "ubs" {
		t.Errorf("Env[ATL_SITE] = %q, want ubs", doc.Envelope.Spec.Env["ATL_SITE"])
	}
	if doc.Envelope.Spec.Capabilities.Docker == nil {
		t.Error("Capabilities.Docker = nil, want non-nil")
	}
	if len(doc.Envelope.Spec.Projects) != 1 || doc.Envelope.Spec.Projects[0].Name != "rook-server" {
		t.Errorf("Projects = %+v", doc.Envelope.Spec.Projects)
	}
	for _, field := range []string{"host_commands", "env", "allowed_domains", "capabilities", "projects"} {
		if !doc.FieldsPresent[field] {
			t.Errorf("FieldsPresent[%q] = false, want true", field)
		}
	}
	if doc.AdditionalBindingsDropped {
		t.Error("AdditionalBindingsDropped = true, want false (key absent)")
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_MultiDocument(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-a
spec:
  host_commands: [gh]
---
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-b
spec:
  host_commands: [aws]
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("len(docs) = %d, want 2", len(docs))
	}
	if docs[0].Envelope.Metadata.Name != "team-a" || docs[1].Envelope.Metadata.Name != "team-b" {
		t.Errorf("names = %q, %q", docs[0].Envelope.Metadata.Name, docs[1].Envelope.Metadata.Name)
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_MissingFieldsAreNotPresent(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  host_commands: [gh]
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	doc := docs[0]
	if !doc.FieldsPresent["host_commands"] {
		t.Error("FieldsPresent[host_commands] = false, want true")
	}
	if doc.FieldsPresent["env"] {
		t.Error("FieldsPresent[env] = true, want false (key absent from source)")
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_EmptyEnvIsPresentButEmpty(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  env: {}
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	doc := docs[0]
	if !doc.FieldsPresent["env"] {
		t.Error("FieldsPresent[env] = false, want true (explicit empty map present)")
	}
	if len(doc.Envelope.Spec.Env) != 0 {
		t.Errorf("Env = %v, want empty", doc.Envelope.Spec.Env)
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_RejectsWrongAPIVersion(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v2
kind: Workspace
metadata:
  name: default
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil || !strings.Contains(err.Error(), "apiVersion") {
		t.Fatalf("expected an apiVersion error, got %v", err)
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_RejectsWrongKind(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Project
metadata:
  name: default
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("expected a kind error, got %v", err)
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_RejectsMissingName(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: ""
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error for empty metadata.name")
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownSpecField(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  hostcommands: [gh]
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil || !strings.Contains(err.Error(), "hostcommands") {
		t.Fatalf("expected an unknown field error mentioning hostcommands, got %v", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_TolerateAdditionalBindings pins the
// plan doc's decision: additional_bindings is dropped from the schema, but a
// document that still carries it (e.g. hand-edited from an old export) must
// not fail to parse — the caller surfaces a warning instead (docs/plans/
// volume-only-daemon.md §論点g「additional_bindings の扱い」).
func TestDecodeWorkspaceEnvelopeDocuments_TolerateAdditionalBindings(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  additional_bindings:
    - source: /host/path
      target: /container/path
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	if !docs[0].AdditionalBindingsDropped {
		t.Error("AdditionalBindingsDropped = false, want true")
	}
}

func TestDecodeWorkspaceEnvelopeDocuments_EmptyBodyIsError(t *testing.T) {
	_, err := DecodeWorkspaceEnvelopeDocuments([]byte(""))
	if err == nil {
		t.Fatal("expected an error for an empty file")
	}
}

// ---------------------------------------------------------------------------
// WorkspaceEnvelopeApply.MergeInto
// ---------------------------------------------------------------------------

func TestMergeInto_MissingFieldPreservesCurrent(t *testing.T) {
	current := &WorkspaceMeta{HostCommands: []string{"gh"}, Env: map[string]string{"FOO": "bar"}}
	doc := &WorkspaceEnvelopeApply{
		Envelope:      &WorkspaceEnvelope{Spec: WorkspaceEnvelopeSpec{AllowedDomains: []string{"example.com"}}},
		FieldsPresent: map[string]bool{"allowed_domains": true},
	}
	merged := doc.MergeInto(current)
	if !equalStrSlice(merged.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want preserved [gh]", merged.HostCommands)
	}
	if merged.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want preserved bar", merged.Env["FOO"])
	}
	if !equalStrSlice(merged.AllowedDomains, []string{"example.com"}) {
		t.Errorf("AllowedDomains = %v, want [example.com]", merged.AllowedDomains)
	}
}

func TestMergeInto_PresentEmptyClears(t *testing.T) {
	current := &WorkspaceMeta{Env: map[string]string{"FOO": "bar"}}
	doc := &WorkspaceEnvelopeApply{
		Envelope:      &WorkspaceEnvelope{Spec: WorkspaceEnvelopeSpec{Env: map[string]string{}}},
		FieldsPresent: map[string]bool{"env": true},
	}
	merged := doc.MergeInto(current)
	if len(merged.Env) != 0 {
		t.Errorf("Env = %v, want cleared", merged.Env)
	}
}

func TestMergeInto_NilCurrentIsCreate(t *testing.T) {
	doc := &WorkspaceEnvelopeApply{
		Envelope:      &WorkspaceEnvelope{Spec: WorkspaceEnvelopeSpec{HostCommands: []string{"gh"}}},
		FieldsPresent: map[string]bool{"host_commands": true},
	}
	merged := doc.MergeInto(nil)
	if !equalStrSlice(merged.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", merged.HostCommands)
	}
}

// ---------------------------------------------------------------------------
// NewWorkspaceEnvelopeFromMeta / round trip
// ---------------------------------------------------------------------------

func TestNewWorkspaceEnvelopeFromMeta_RoundTrip(t *testing.T) {
	meta := &WorkspaceMeta{
		HostCommands:   []string{"gh"},
		Env:            map[string]string{"FOO": "bar"},
		AllowedDomains: []string{"example.com"},
	}
	projects := []WorkspaceEnvelopeProject{{Name: "rook-server", URL: "git@bitbucket.org:x/rook-server.git"}}
	envelope := NewWorkspaceEnvelopeFromMeta("default", meta, projects)
	if envelope.APIVersion != WorkspaceEnvelopeAPIVersion {
		t.Errorf("APIVersion = %q", envelope.APIVersion)
	}
	if envelope.Kind != WorkspaceEnvelopeKind {
		t.Errorf("Kind = %q", envelope.Kind)
	}

	data, err := yaml.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments round trip: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("len(docs) = %d, want 1", len(docs))
	}
	got := docs[0].Envelope
	if got.Metadata.Name != "default" {
		t.Errorf("Metadata.Name = %q", got.Metadata.Name)
	}
	if !equalStrSlice(got.Spec.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v", got.Spec.HostCommands)
	}
	if len(got.Spec.Projects) != 1 || got.Spec.Projects[0].URL != "git@bitbucket.org:x/rook-server.git" {
		t.Errorf("Projects = %+v", got.Spec.Projects)
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
