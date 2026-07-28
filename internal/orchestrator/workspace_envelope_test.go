package orchestrator

import (
	"reflect"
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

// TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownProjectField pins the
// PR-1d codex round-2 Major "strict decoding doesn't extend to
// spec.projects[]" finding: a typo'd key inside a projects[] entry (here
// "nam" instead of "name") must be rejected the same way an unknown
// top-level or spec-level field already is — not silently decoded into a
// project with an empty Name, which ResolveProjectRef's substring-match
// fallback (Contains(x, "") is always true) would then match against
// whichever project happens to be registered.
func TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownProjectField(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  projects:
    - nam: intended
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil || !strings.Contains(err.Error(), "nam") {
		t.Fatalf("expected an unknown field error mentioning \"nam\", got %v", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_RejectsEmptyProjectName is the other
// half of the same round-2 finding: even a well-formed document (no unknown
// keys) with an explicitly empty/blank projects[].name must be rejected —
// ResolveProjectRef("") would otherwise match every registered project via
// its substring-match fallback.
func TestDecodeWorkspaceEnvelopeDocuments_RejectsEmptyProjectName(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  projects:
    - name: ""
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("expected a \"name is required\" error, got %v", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownCapabilitiesField is
// the capabilities-side counterpart: a typo'd key under spec.capabilities
// (here "dcoker" instead of "docker") must be rejected rather than silently
// decoded into a zero-value Capabilities that quietly disables Docker.
func TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownCapabilitiesField(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
spec:
  capabilities:
    dcoker: {}
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil || !strings.Contains(err.Error(), "dcoker") {
		t.Fatalf("expected an unknown field error mentioning \"dcoker\", got %v", err)
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

// TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownTopLevelField pins
// MAJOR 2 (codex round-1): a typo'd top-level field (or a mis-indented
// spec.projects that landed one level up) must be rejected, not silently
// dropped.
func TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownTopLevelField(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: default
projectz:
  - name: oops
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error for the unknown top-level field \"projectz\"")
	}
	if !strings.Contains(err.Error(), "projectz") {
		t.Errorf("error = %v, want it to mention projectz", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_BareMetaDocumentGuidesToCreateOrEdit
// pins bareMetaSentToTheWrongDoor: a bare workspace-meta document (no
// apiVersion/kind, host_commands:/env:/... at the top level — what `boid
// workspace create --from-file`/`edit --from-file` take, and what a
// hand-authored yaml or `boid project migrate`'s shadow file usually is)
// fed to `apply` must be redirected to those two commands by name, not left
// to the underlying decoder's raw "field host_commands not found in type
// orchestrator.rawWorkspaceEnvelope" error — the same UX gap
// envelopeSentToTheWrongDoor (workspace_meta_strict.go) already closed for
// the opposite direction (a `workspace export` envelope fed to
// create/edit), found 2026-07-28 while retiring `boid workspace import`
// (whose deprecation message points here for envelope-shaped files, but
// most of its actual callers' files are this bare shape instead).
func TestDecodeWorkspaceEnvelopeDocuments_BareMetaDocumentGuidesToCreateOrEdit(t *testing.T) {
	data := []byte("host_commands:\n  - gh\nenv:\n  FOO: bar\n")
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error for a bare meta document")
	}
	if strings.Contains(err.Error(), "rawWorkspaceEnvelope") {
		t.Errorf("error = %v, must not leak the unexported Go type name", err)
	}
	if !strings.Contains(err.Error(), "boid workspace create") || !strings.Contains(err.Error(), "boid workspace edit") {
		t.Errorf("error = %v, want it to point at both `boid workspace create --from-file` and `edit --from-file`", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_BareMetaDocumentWithSlugKeyAlsoGuides
// covers the specific shape the (now-retired) GET /api/workspaces/{slug}/export
// endpoint used to produce — a bare meta document with a top-level "slug:"
// key spliced on — landing at `apply` instead of `create`/`edit --from-file`.
func TestDecodeWorkspaceEnvelopeDocuments_BareMetaDocumentWithSlugKeyAlsoGuides(t *testing.T) {
	data := []byte("slug: default\nenv:\n  FOO: bar\n")
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error for a bare meta document carrying a slug key")
	}
	if !strings.Contains(err.Error(), "boid workspace create") || !strings.Contains(err.Error(), "boid workspace edit") {
		t.Errorf("error = %v, want it to point at both `boid workspace create --from-file` and `edit --from-file`", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_UnrelatedTypoDoesNotTriggerTheGuide
// pins bareMetaSentToTheWrongDoor's own doc comment: a document with NEITHER
// an envelope's identifying keys NOR any recognizable WorkspaceMeta field
// name must fall through to the original decode error unchanged — the guide
// is specifically for the bare-meta shape, not a catch-all for anything
// unparsable.
func TestDecodeWorkspaceEnvelopeDocuments_UnrelatedTypoDoesNotTriggerTheGuide(t *testing.T) {
	data := []byte("totally: bogus\n")
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "bare workspace meta document") {
		t.Errorf("error = %v, the bare-meta guide must not fire for an unrelated typo", err)
	}
	if !strings.Contains(err.Error(), "totally") {
		t.Errorf("error = %v, want the original unknown-field error mentioning \"totally\"", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_ValidEnvelopeIsUnaffectedByTheGuide
// pins that a well-formed envelope document (apiVersion/kind present) is
// unaffected by bareMetaSentToTheWrongDoor even though its own decode error
// case (an unknown spec field) is exercised elsewhere
// (TestDecodeWorkspaceEnvelopeDocuments_RejectsUnknownSpecField) — this one
// specifically pins the SUCCESS path still works with the new check wired in.
func TestDecodeWorkspaceEnvelopeDocuments_ValidEnvelopeIsUnaffectedByTheGuide(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-a
spec:
  host_commands: [gh]
`)
	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	if len(docs) != 1 || docs[0].Envelope.Metadata.Name != "team-a" {
		t.Fatalf("docs = %+v, want one document named team-a", docs)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_OldEmptyExportIsAPlainSyntaxError pins
// a known, ACCEPTED gap in bareMetaSentToTheWrongDoor's guidance, not a bug:
// the exact byte sequence the now-retired GET /api/workspaces/{slug}/export
// used to produce for an EMPTY workspace — "slug: default\n{}\n", the
// concatenation of a spliced "slug:" line with a zero-value WorkspaceMeta's
// "{}" marshal — is not valid yaml at all (a "{}" flow mapping cannot follow
// a completed key: value pair at the same indentation), so
// bareMetaSentToTheWrongDoor's own `yaml.Unmarshal(data, &doc)` fails
// exactly like DecodeWorkspaceEnvelopeDocuments' own decode does, and the
// guide's `if err != nil { return nil }` branch (see its doc comment) falls
// through to the raw syntax error untouched. Fixing this would require a
// second, more tolerant parse attempt purely to improve an error message for
// a byte sequence NOTHING can produce any more (the endpoint that wrote it
// is deleted) — not worth the complexity. If this test ever starts failing
// because the error becomes friendly, that is a welcome surprise, not a
// break; if it fails because bareMetaSentToTheWrongDoor started PANICKING or
// mis-detecting on malformed input, that is the regression to investigate.
func TestDecodeWorkspaceEnvelopeDocuments_OldEmptyExportIsAPlainSyntaxError(t *testing.T) {
	data := []byte("slug: default\n{}\n")
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error for the old empty-export byte sequence")
	}
	if !strings.Contains(err.Error(), "could not find expected") {
		t.Errorf("error = %v, want a plain yaml syntax error (not bareMetaSentToTheWrongDoor's guidance — this input can't reach it, see doc comment)", err)
	}
}

// TestDecodeWorkspaceEnvelopeDocuments_MultiDocumentBareMetaSecondDocLeaksTypeName
// pins the other known, accepted gap bareMetaSentToTheWrongDoor's own doc
// comment describes: in a multi-document apply file, the guide only probes
// the FIRST document's shape (yaml.Unmarshal's own behavior; see the
// function's doc comment for why re-probing per-document was judged not
// worth it). When document 1 is a valid envelope and a LATER document is the
// bare-meta one, the raw unexported-type-name error still leaks for that
// document — this test exists so that gap stays a documented, deliberate
// trade-off instead of silently changing shape (e.g. if a future refactor
// accidentally started probing every document and this test caught it
// passing instead of failing, that would be worth a second look, not
// necessarily a problem).
func TestDecodeWorkspaceEnvelopeDocuments_MultiDocumentBareMetaSecondDocLeaksTypeName(t *testing.T) {
	data := []byte(`apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-a
spec:
  host_commands: [gh]
---
host_commands: [aws]
`)
	_, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err == nil {
		t.Fatal("expected an error for the second, bare-meta document")
	}
	if !strings.Contains(err.Error(), "document 2") {
		t.Errorf("error = %v, want it to name document 2", err)
	}
	if !strings.Contains(err.Error(), "rawWorkspaceEnvelope") {
		t.Errorf("error = %v, want the raw unexported-type-name error (known accepted gap, see doc comment) since only document 1 is probed", err)
	}
}

// TestBareMetaKnownFieldNames_CoversEveryWorkspaceMetaField is the drift
// guard bareMetaKnownFieldNames' own doc comment promises: every yaml-tagged
// field of WorkspaceMeta (workspace_meta.go) must have a corresponding entry
// in bareMetaKnownFieldNames, found here via reflection rather than by
// re-listing the fields by hand (the same class of drift a hand-maintained
// second list could silently fall behind on). Without this, adding a new
// WorkspaceMeta field would silently narrow bareMetaSentToTheWrongDoor's
// detection — a bare-meta document that only sets the new field would fall
// through to the raw "field ... not found in type
// orchestrator.rawWorkspaceEnvelope" error this guard exists to replace —
// with nothing failing to flag the gap. bareMetaKnownFieldNames is
// deliberately allowed to carry MORE entries than WorkspaceMeta has fields
// ("slug", "additional_bindings" — see its own doc comment for why), so this
// only asserts one direction: every WorkspaceMeta field name must appear in
// bareMetaKnownFieldNames, not the reverse.
func TestBareMetaKnownFieldNames_CoversEveryWorkspaceMetaField(t *testing.T) {
	typ := reflect.TypeOf(WorkspaceMeta{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" {
			t.Fatalf("WorkspaceMeta field %s has no yaml tag; bareMetaKnownFieldNames cannot be checked against it", f.Name)
		}
		// Strip yaml tag options (e.g. "host_commands,omitempty" -> "host_commands").
		name := strings.SplitN(tag, ",", 2)[0]
		if !bareMetaKnownFieldNames[name] {
			t.Errorf("WorkspaceMeta field %s (yaml key %q) is missing from bareMetaKnownFieldNames — "+
				"add it there so bareMetaSentToTheWrongDoor still recognizes a bare-meta document that only sets this field",
				f.Name, name)
		}
	}
}

// ---------------------------------------------------------------------------
// SplitWorkspaceEnvelopeDocuments
// ---------------------------------------------------------------------------

func TestSplitWorkspaceEnvelopeDocuments_PreservesFieldPresence(t *testing.T) {
	data := []byte(`
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-a
spec:
  env: {}
  host_commands: [gh]
---
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-b
`)
	parts, err := SplitWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("SplitWorkspaceEnvelopeDocuments: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}

	docs0, err := DecodeWorkspaceEnvelopeDocuments(parts[0])
	if err != nil {
		t.Fatalf("decode part 0: %v", err)
	}
	if docs0[0].Envelope.Metadata.Name != "team-a" {
		t.Errorf("part 0 name = %q, want team-a", docs0[0].Envelope.Metadata.Name)
	}
	if !docs0[0].FieldsPresent["env"] {
		t.Error("part 0 FieldsPresent[env] = false, want true (explicit env: {} preserved)")
	}
	if len(docs0[0].Envelope.Spec.Env) != 0 {
		t.Errorf("part 0 Env = %v, want empty", docs0[0].Envelope.Spec.Env)
	}

	docs1, err := DecodeWorkspaceEnvelopeDocuments(parts[1])
	if err != nil {
		t.Fatalf("decode part 1: %v", err)
	}
	if docs1[0].Envelope.Metadata.Name != "team-b" {
		t.Errorf("part 1 name = %q, want team-b", docs1[0].Envelope.Metadata.Name)
	}
	if docs1[0].FieldsPresent["env"] {
		t.Error("part 1 FieldsPresent[env] = true, want false (spec: absent entirely)")
	}
}

func TestSplitWorkspaceEnvelopeDocuments_EmptyBodyIsError(t *testing.T) {
	_, err := SplitWorkspaceEnvelopeDocuments([]byte(""))
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
	envelope := NewWorkspaceEnvelopeFromMeta("default", meta, projects, "")
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

// TestNewWorkspaceEnvelopeFromMeta_ExplicitEmptyFieldsSurviveExport pins
// Blocker 1 (codex round-1): an export of a workspace with an explicitly
// empty env / no projects must NOT collapse those fields to "absent" —
// omitempty previously made "env: {}" and "projects: []" indistinguishable
// from the key never having been written at all, which meant an apply of
// that exported document could never clear a workspace's env or detach its
// projects (FieldsPresent would read false for both).
func TestNewWorkspaceEnvelopeFromMeta_ExplicitEmptyFieldsSurviveExport(t *testing.T) {
	meta := &WorkspaceMeta{
		Env:            map[string]string{},
		HostCommands:   []string{},
		AllowedDomains: []string{},
	}
	envelope := NewWorkspaceEnvelopeFromMeta("team-empty", meta, []WorkspaceEnvelopeProject{}, "")
	data, err := yaml.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, want := range []string{"env: {}", "host_commands: []", "allowed_domains: []", "projects: []"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("exported yaml = %q, want it to contain %q", string(data), want)
		}
	}

	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments round trip: %v", err)
	}
	doc := docs[0]
	for _, field := range []string{"env", "host_commands", "allowed_domains", "projects"} {
		if !doc.FieldsPresent[field] {
			t.Errorf("FieldsPresent[%q] = false, want true (explicit empty value must round-trip as present)", field)
		}
	}
}

// TestNewWorkspaceEnvelopeFromMeta_ExtraFieldsSurviveExport is round-2's
// follow-up to TestNewWorkspaceEnvelopeFromMeta_ExplicitEmptyFieldsSurviveExport
// above: round-1 dropped `omitempty` from host_commands/env/allowed_domains/
// projects but MISSED extra_repos/container_image/capabilities, so exporting
// a workspace with no extra repos and Docker disabled silently omitted those
// three keys instead of emitting an explicit empty value — see
// TestApplyWorkspaceEnvelope_ClearsStaleExtraReposAndCapabilities below for
// the full export -> apply round trip this enables.
func TestNewWorkspaceEnvelopeFromMeta_ExtraFieldsSurviveExport(t *testing.T) {
	meta := &WorkspaceMeta{
		ExtraRepos:     []string{},
		ContainerImage: "",
		Capabilities:   Capabilities{},
	}
	envelope := NewWorkspaceEnvelopeFromMeta("team-empty", meta, nil, "")
	data, err := yaml.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, want := range []string{"extra_repos: []", `container_image: ""`, "capabilities: {}"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("exported yaml = %q, want it to contain %q", string(data), want)
		}
	}

	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments round trip: %v", err)
	}
	doc := docs[0]
	for _, field := range []string{"extra_repos", "container_image", "capabilities"} {
		if !doc.FieldsPresent[field] {
			t.Errorf("FieldsPresent[%q] = false, want true (explicit empty value must round-trip as present)", field)
		}
	}
}

// TestApplyWorkspaceEnvelope_ClearsStaleExtraReposAndCapabilities pins
// Blocker 1's extension (PR-1d codex round-2): exporting a workspace with
// `extra_repos: []` and `capabilities: {}` and then applying that export
// over a DIFFERENT workspace that still has a stale extra_repos entry and
// capabilities.docker enabled must clear both — not silently leave them, as
// it did before the omitempty tags were dropped from these three fields.
func TestApplyWorkspaceEnvelope_ClearsStaleExtraReposAndCapabilities(t *testing.T) {
	// Export side: a workspace with both fields explicitly empty.
	cleanMeta := &WorkspaceMeta{ExtraRepos: []string{}, Capabilities: Capabilities{}}
	envelope := NewWorkspaceEnvelopeFromMeta("team-a", cleanMeta, nil, "")
	data, err := yaml.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	docs, err := DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		t.Fatalf("DecodeWorkspaceEnvelopeDocuments: %v", err)
	}
	apply := docs[0]

	// Apply side: an existing workspace with stale extra_repos + Docker
	// capability enabled.
	stale := &WorkspaceMeta{
		ExtraRepos:   []string{"git@example.com:private/leftover.git"},
		Capabilities: Capabilities{Docker: &DockerCapability{}},
	}
	merged := apply.MergeInto(stale)
	if len(merged.ExtraRepos) != 0 {
		t.Errorf("ExtraRepos = %v, want cleared", merged.ExtraRepos)
	}
	if merged.Capabilities.Docker != nil {
		t.Errorf("Capabilities.Docker = %+v, want cleared (nil)", merged.Capabilities.Docker)
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
