package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file implements the K8s-like workspace export/apply document shape
// (docs/plans/volume-only-daemon.md §論点 g「workspace/project export/import
// shape」): a single self-describing YAML document (or several, "---"-
// separated) that carries a workspace's WorkspaceMeta fields plus the
// project names/URLs assigned to it. `boid workspace export`/`apply` (cmd/
// workspace_export.go, cmd/workspace_apply.go) are the only callers; the
// plan doc declares `workspace export --all` the sole endorsed backup path
// going forward (§論点g「決断: backup 契約」) — this is a fresh, additive
// shape, not a replacement for the pre-existing revision/ETag-based
// WorkspaceMeta wire format that Create/Update/CreateStrict above still use
// for the single-resource POST/PUT bodies apply constructs under the hood.

const (
	// WorkspaceEnvelopeAPIVersion is the only apiVersion value this binary
	// accepts. A future incompatible schema change bumps this (e.g.
	// boid.dev/v2) rather than silently reinterpreting an old document.
	WorkspaceEnvelopeAPIVersion = "boid.dev/v1"
	// WorkspaceEnvelopeKind is the only kind value this binary accepts.
	WorkspaceEnvelopeKind = "Workspace"
)

// WorkspaceEnvelope is the export/apply document shape: an apiVersion/kind/
// metadata/spec envelope around a workspace's meta + project assignments.
type WorkspaceEnvelope struct {
	APIVersion string                    `yaml:"apiVersion"`
	Kind       string                    `yaml:"kind"`
	Metadata   WorkspaceEnvelopeMetadata `yaml:"metadata"`
	Spec       WorkspaceEnvelopeSpec     `yaml:"spec"`
}

// WorkspaceEnvelopeMetadata holds the envelope's identifying field: the
// workspace slug (workspaces.id / WorkspaceSummary.ID).
type WorkspaceEnvelopeMetadata struct {
	Name string `yaml:"name"`
}

// WorkspaceEnvelopeSpec mirrors WorkspaceMeta field-for-field (see that
// type's doc comment for what each field means at runtime) plus Projects,
// which has no WorkspaceMeta equivalent — project-workspace assignment is
// separate bookkeeping (project_workspaces / Project.WorkspaceID), not part
// of the workspace's own meta row.
//
// additional_bindings is deliberately NOT a field here: it was retired in
// docs/plans/home-workspace-volume.md Phase 4 PR4 and the plan doc for this
// package (§論点g) explicitly excludes it from the export schema. A source
// document that still carries a spec.additional_bindings key is tolerated
// by decodeWorkspaceEnvelopeSpec (see below) — parsed and discarded, with
// WorkspaceEnvelopeApply.AdditionalBindingsDropped set so the caller can
// warn — rather than rejected as an unknown field.
// None of spec's fields carry `omitempty` (PR-1d codex round-1 Blocker 1,
// extended to every remaining field by round-2's follow-up finding): missing
// vs. explicitly-empty is a load-bearing distinction for every one of these
// seven (WorkspaceEnvelopeApply's FieldsPresent-driven merge, and — for
// projects — ApplyWorkspaceEnvelope's attach/detach reconciliation), and
// `omitempty` on export would silently collapse "the workspace's field
// really is empty/zero" into "the field was absent from the document",
// which on a subsequent apply is indistinguishable from "leave the current
// value untouched" — an export → apply round trip could then never actually
// clear a field back to empty (round-2's concrete failure: exporting a
// workspace with no extra_repos and Docker disabled, then applying that
// backup over a workspace with a stale extra_repos entry and
// capabilities.docker enabled, left both untouched because the omitempty
// tags dropped the keys from the export entirely). yaml.v3 marshals a
// nil/empty slice, map, or zero-value struct/string as `[]`/`{}`/`""` either
// way (verified: no `omitempty` needed to avoid a `null` literal here), so
// dropping the tag is a pure presence-fidelity fix with no representation
// change for the non-empty case.
type WorkspaceEnvelopeSpec struct {
	HostCommands   []string                   `yaml:"host_commands"`
	Env            map[string]string          `yaml:"env"`
	AllowedDomains []string                   `yaml:"allowed_domains"`
	ExtraRepos     []string                   `yaml:"extra_repos"`
	ContainerImage string                     `yaml:"container_image"`
	Capabilities   Capabilities               `yaml:"capabilities"`
	Projects       []WorkspaceEnvelopeProject `yaml:"projects"`
	// InitScript is the workspace's init.sh, verbatim (PR9 of
	// docs/plans/workspace-home-volume-persistence.md, 論点 d).
	//
	// Like Projects it has NO WorkspaceMeta counterpart, and for a sharper
	// reason: init.sh is a FILE the daemon owns (dispatcher's
	// workspaceInitScriptPath), deliberately not a workspaces-table column —
	// see that function's doc comment for the standing decision (environment-
	// dependent shell content, outside the workspace's otherwise
	// environment-independent DB-backed config). It is carried here anyway
	// because an export that omits it is not a backup: restoring a workspace
	// without its init.sh gives you a home volume with no toolchain in it and
	// a harness that cannot start.
	//
	// MergeInto therefore does not touch it — the hydration happens in
	// internal/api, which holds the store (ProjectAppService.ApplyWorkspace).
	//
	// Missing vs. explicitly-empty is load-bearing exactly as it is for the
	// fields above: no key means "leave this workspace's init.sh alone", and
	// `init_script: ""` means "this workspace has no init.sh", which on apply
	// deletes the target's. An empty script and no script are the same state
	// by construction — the dispatch wrapper writes the bytes to a file and
	// runs bash on it, so zero bytes is a no-op run — and collapsing them
	// keeps a workspace from acquiring a completion-marker hash that describes
	// nothing.
	InitScript string `yaml:"init_script"`
}

// WorkspaceEnvelopeProject is one spec.projects[] entry. URL is informational
// only until PR-2 lands `boid project add <url>` (docs/plans/
// volume-only-daemon.md §論点g): apply does not register a project from URL,
// it only attaches an already-registered project (matched by name) to the
// workspace.
type WorkspaceEnvelopeProject struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url,omitempty"`
}

// workspaceEnvelopeSpecFields is the allow-list of spec.* keys a document
// may carry. additional_bindings is listed so it is tolerated (parsed into
// AdditionalBindingsDropped, then discarded) rather than rejected outright,
// matching workspace_meta_strict.go's existing tolerate-and-warn precedent
// for the very same retired field on the lower-level WorkspaceMeta wire path.
// Anything else is rejected with "unknown field" — the same typo-guard
// rationale as workspace_meta_strict.go's DecodeWorkspaceMetaStrict.
var workspaceEnvelopeSpecFields = map[string]bool{
	"host_commands":       true,
	"env":                 true,
	"allowed_domains":     true,
	"extra_repos":         true,
	"container_image":     true,
	"capabilities":        true,
	"projects":            true,
	"init_script":         true,
	"additional_bindings": true,
}

// WorkspaceEnvelopeApply is the result of decoding one Workspace document
// for `boid workspace apply`: the parsed envelope plus enough bookkeeping
// for the K8s-style "missing field = don't touch, present-but-empty = clear"
// merge semantics (docs/plans/volume-only-daemon.md, PR-1d unilateral
// decision) that a plain *WorkspaceEnvelope alone cannot represent — a zero-
// value Go slice/map cannot be told apart from "the key was absent" once
// decoded into a plain struct.
type WorkspaceEnvelopeApply struct {
	Envelope *WorkspaceEnvelope
	// FieldsPresent records which spec.* keys were present in the source
	// document (by their yaml key name: "host_commands", "env",
	// "allowed_domains", "extra_repos", "container_image", "capabilities",
	// "projects", "init_script"). A key absent from this map was not present
	// in the document at all and MergeInto leaves the corresponding
	// WorkspaceMeta field untouched; a key present (even if its decoded value
	// is the zero value / an explicit empty list or map) means the document
	// explicitly wants that field replaced with the decoded value.
	//
	// "init_script" is in this map but not in MergeInto's switch: it has no
	// WorkspaceMeta field to merge into (see WorkspaceEnvelopeSpec.InitScript).
	// Its consumer is ProjectAppService.ApplyWorkspace, which reads the same
	// flag to decide between "leave the file alone" and "write/clear it".
	FieldsPresent map[string]bool
	// AdditionalBindingsDropped is true when the source document had a
	// spec.additional_bindings key (any shape, including an explicit empty
	// list or null) — retired per docs/plans/volume-only-daemon.md §論点g;
	// the value is always discarded. Callers should log/print a warning
	// rather than fail the apply.
	AdditionalBindingsDropped bool
}

// NewWorkspaceEnvelopeFromMeta builds a WorkspaceEnvelope for `boid workspace
// export` from a workspace's current WorkspaceMeta, its resolved project
// entries (name + url, url may be empty pre-PR-2 — see
// WorkspaceEnvelopeProject's doc comment) and its init.sh. meta may be nil (an
// empty/never-configured workspace), in which case the spec carries only
// projects (if any) and the script.
//
// initScript is a plain string rather than a (content, exists) pair because
// the two states an init.sh can be in are "some bytes" and "none", and the
// empty string is how the envelope spells the second one — see
// WorkspaceEnvelopeSpec.InitScript. A caller with nothing to export passes "",
// which is emitted as `init_script: ""` and read back as an explicit clear.
func NewWorkspaceEnvelopeFromMeta(name string, meta *WorkspaceMeta, projects []WorkspaceEnvelopeProject, initScript string) *WorkspaceEnvelope {
	spec := WorkspaceEnvelopeSpec{Projects: projects, InitScript: initScript}
	if meta != nil {
		spec.HostCommands = meta.HostCommands
		spec.Env = meta.Env
		spec.AllowedDomains = meta.AllowedDomains
		spec.ExtraRepos = meta.ExtraRepos
		spec.ContainerImage = meta.ContainerImage
		spec.Capabilities = meta.Capabilities
	}
	return &WorkspaceEnvelope{
		APIVersion: WorkspaceEnvelopeAPIVersion,
		Kind:       WorkspaceEnvelopeKind,
		Metadata:   WorkspaceEnvelopeMetadata{Name: name},
		Spec:       spec,
	}
}

// MergeInto applies a's spec onto current (which may be nil — no existing
// workspace, i.e. a `boid workspace apply` create rather than update) per
// field, honoring FieldsPresent's "missing = don't touch" contract, and
// returns the resulting *WorkspaceMeta. current is never mutated.
//
// spec.init_script is deliberately absent from the field list below: it is a
// file on the daemon, not a workspaces-table column, so there is nothing on
// *WorkspaceMeta to merge it into (WorkspaceEnvelopeSpec.InitScript). Its
// FieldsPresent flag is honored by ProjectAppService.ApplyWorkspace, outside
// this function and outside the DB transaction — a file write and a
// transaction cannot be made atomic, and pretending otherwise by routing it
// through here would only hide that.
func (a *WorkspaceEnvelopeApply) MergeInto(current *WorkspaceMeta) *WorkspaceMeta {
	merged := &WorkspaceMeta{}
	if current != nil {
		*merged = *current
	}
	spec := a.Envelope.Spec
	if a.FieldsPresent["host_commands"] {
		merged.HostCommands = spec.HostCommands
	}
	if a.FieldsPresent["env"] {
		merged.Env = spec.Env
	}
	if a.FieldsPresent["allowed_domains"] {
		merged.AllowedDomains = spec.AllowedDomains
	}
	if a.FieldsPresent["extra_repos"] {
		merged.ExtraRepos = spec.ExtraRepos
	}
	if a.FieldsPresent["container_image"] {
		merged.ContainerImage = spec.ContainerImage
	}
	if a.FieldsPresent["capabilities"] {
		merged.Capabilities = spec.Capabilities
	}
	return merged
}

// DecodeWorkspaceEnvelopeDocuments decodes data as one or more "---"-
// separated Workspace envelope documents (docs/plans/volume-only-daemon.md
// §論点g: "複数 workspace は YAML の --- 区切りで 1 file にまとめられる").
// An empty/whitespace-only body is an error (unlike DecodeWorkspaceMetaStrict
// — an apply file with nothing in it is almost certainly a mistake, not a
// legitimate "no-op apply"). Every document must independently satisfy:
//   - apiVersion == WorkspaceEnvelopeAPIVersion
//   - kind == WorkspaceEnvelopeKind
//   - metadata.name is non-empty and a valid workspace slug
//   - spec has no unknown fields (additional_bindings tolerated — see
//     workspaceEnvelopeSpecFields)
func DecodeWorkspaceEnvelopeDocuments(data []byte) ([]*WorkspaceEnvelopeApply, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("empty document: expected at least one apiVersion/kind/metadata/spec Workspace document")
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	// MAJOR 2 (codex round-1): reject an unknown TOP-LEVEL field (a typo'd
	// "projectz:", or a mis-indented "projects:" that landed one level up
	// from spec:) instead of silently dropping it. This only governs
	// rawWorkspaceEnvelope's own struct fields (apiVersion/kind/metadata/
	// spec) — spec's own allow-list (workspaceEnvelopeSpecFields, including
	// the additional_bindings tolerance) is enforced separately below by
	// decodeWorkspaceEnvelopeSpec, which decodes spec into a raw
	// map[string]yaml.Node and is unaffected by this decoder-level setting
	// (KnownFields only applies to struct-tagged decode targets).
	dec.KnownFields(true)
	var docs []*WorkspaceEnvelopeApply
	for i := 0; ; i++ {
		doc, err := decodeOneWorkspaceEnvelope(dec)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if bmErr := bareMetaSentToTheWrongDoor(data); bmErr != nil {
				return nil, bmErr
			}
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// bareMetaSentToTheWrongDoor is the reciprocal of workspace_meta_strict.go's
// envelopeSentToTheWrongDoor: it detects a BARE workspace-meta document (no
// apiVersion/kind, host_commands:/env:/... at the top level — what `boid
// workspace create --from-file`/`edit --from-file` take, and what a
// hand-authored workspace yaml or `boid project migrate`'s shadow file
// usually is) landing at `apply`'s decoder instead of one of those.
//
// Without this, the KnownFields unmarshal above fails with e.g. "document 1:
// yaml: unmarshal errors:\n  line 1: field host_commands not found in type
// orchestrator.rawWorkspaceEnvelope" — an unexported Go type name that
// describes the document by what it is not and never mentions the
// one-word-different commands that actually accept this shape. Found
// 2026-07-28 alongside `boid workspace import`'s retirement: that command
// used to be the one place a bare meta document reliably worked end to end
// (however brokenly — see cmd/workspace.go's runWorkspaceImportDeprecated),
// so pointing its deprecation message at `apply -f` would otherwise dead-end
// on this exact error for the common case (a bare meta file, not an
// envelope) — the same gap envelopeSentToTheWrongDoor closed for the
// opposite direction (docs/plans/workspace-home-volume-persistence.md 論点
// d, 2026-07-28 dogfood).
//
// Detection is deliberately loose, matching envelopeSentToTheWrongDoor's own
// posture: apiVersion and kind both absent, AND at least one field name
// WorkspaceMeta actually has is present at the top level. A document with
// neither an envelope's identifying keys nor any recognizable meta field
// (e.g. a typo of everything) falls through to the original decode error,
// which is exactly as informative for that case as it always was.
func bareMetaSentToTheWrongDoor(data []byte) error {
	var probe struct {
		APIVersion         string    `yaml:"apiVersion"`
		Kind               string    `yaml:"kind"`
		Slug               yaml.Node `yaml:"slug"`
		Env                yaml.Node `yaml:"env"`
		Capabilities       yaml.Node `yaml:"capabilities"`
		AllowedDomains     yaml.Node `yaml:"allowed_domains"`
		ExtraRepos         yaml.Node `yaml:"extra_repos"`
		HostCommands       yaml.Node `yaml:"host_commands"`
		ContainerImage     yaml.Node `yaml:"container_image"`
		AdditionalBindings yaml.Node `yaml:"additional_bindings"`
	}
	// Loose (no KnownFields): unmarshal only ever reads the FIRST "---"
	// document, which is fine here — a multi-document apply file where a
	// LATER document is the malformed one falls through to the original
	// error below rather than misreporting based on document 1's shape, the
	// same conservative trade-off envelopeSentToTheWrongDoor's own
	// single-document probe makes.
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}
	if probe.APIVersion != "" || probe.Kind != "" {
		return nil
	}
	if probe.Slug.Kind == 0 && probe.Env.Kind == 0 && probe.Capabilities.Kind == 0 &&
		probe.AllowedDomains.Kind == 0 && probe.ExtraRepos.Kind == 0 && probe.HostCommands.Kind == 0 &&
		probe.ContainerImage.Kind == 0 && probe.AdditionalBindings.Kind == 0 {
		return nil
	}
	return errors.New(
		"this looks like a bare workspace meta document (no apiVersion/kind), which `apply` does not take — " +
			"it wants a boid.dev/v1 Workspace envelope (what `boid workspace export` writes). Use " +
			"`boid workspace create <slug> --from-file <file>` for a brand-new workspace, or " +
			"`boid workspace edit <slug> --from-file <file>` for an existing one, instead")
}

// SplitWorkspaceEnvelopeDocuments splits data (one or more "---"-separated
// yaml documents) into the raw bytes of each individual document, WITHOUT
// going through WorkspaceEnvelope's Go struct representation. `boid
// workspace apply` uses this (rather than re-marshaling the
// *WorkspaceEnvelopeApply values DecodeWorkspaceEnvelopeDocuments returns)
// to forward each document's original field-presence exactly as written to
// POST /api/workspaces/apply, one request per document: since
// WorkspaceEnvelopeSpec's fields no longer carry `omitempty` (Blocker 1),
// every zero-value Go field — including one that was never present in the
// source at all — marshals identically to an explicit empty value, so
// re-marshaling the decoded struct would erase exactly the missing-vs-empty
// distinction the fix exists to preserve. Round-tripping through yaml.Node
// instead doesn't have that problem: a mapping key that was absent from the
// source has no corresponding child node at all, so re-marshaling the node
// reproduces the source's exact key set, not a struct's zero-value view of
// it.
func SplitWorkspaceEnvelopeDocuments(data []byte) ([][]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("empty document: expected at least one apiVersion/kind/metadata/spec Workspace document")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out [][]byte
	for i := 0; ; i++ {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		// Before re-emitting: make every scalar's style one this document's
		// own reader can take back (PR9 codex round 2, Major 3). The decode
		// above preserved each scalar's source style, and yaml.v3 cannot
		// re-emit some of those correctly — see forceRoundTrippableScalars.
		forceRoundTrippableScalars(&node)
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("document %d: re-marshal: %w", i+1, err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// rawWorkspaceEnvelope is the outer envelope shape, decoded with spec left
// as a raw yaml.Node so decodeWorkspaceEnvelopeSpec can both validate its
// keys against the allow-list and track which ones were present.
type rawWorkspaceEnvelope struct {
	APIVersion string                    `yaml:"apiVersion"`
	Kind       string                    `yaml:"kind"`
	Metadata   WorkspaceEnvelopeMetadata `yaml:"metadata"`
	Spec       yaml.Node                 `yaml:"spec"`
}

func decodeOneWorkspaceEnvelope(dec *yaml.Decoder) (*WorkspaceEnvelopeApply, error) {
	var raw rawWorkspaceEnvelope
	if err := dec.Decode(&raw); err != nil {
		return nil, err // io.EOF propagates to the caller's loop terminator.
	}

	if raw.APIVersion != WorkspaceEnvelopeAPIVersion {
		return nil, fmt.Errorf("unsupported apiVersion %q (want %q)", raw.APIVersion, WorkspaceEnvelopeAPIVersion)
	}
	if raw.Kind != WorkspaceEnvelopeKind {
		return nil, fmt.Errorf("unsupported kind %q (want %q)", raw.Kind, WorkspaceEnvelopeKind)
	}
	if raw.Metadata.Name == "" {
		return nil, errors.New("metadata.name is required")
	}
	if err := ValidWorkspaceSlug(raw.Metadata.Name); err != nil {
		return nil, fmt.Errorf("metadata.name: %w", err)
	}

	spec, fieldsPresent, additionalBindingsDropped, err := decodeWorkspaceEnvelopeSpec(raw.Spec)
	if err != nil {
		return nil, err
	}

	return &WorkspaceEnvelopeApply{
		Envelope: &WorkspaceEnvelope{
			APIVersion: raw.APIVersion,
			Kind:       raw.Kind,
			Metadata:   raw.Metadata,
			Spec:       spec,
		},
		FieldsPresent:             fieldsPresent,
		AdditionalBindingsDropped: additionalBindingsDropped,
	}, nil
}

// decodeWorkspaceEnvelopeSpec decodes specNode (the raw spec: yaml.Node) into
// a WorkspaceEnvelopeSpec, an explicit-presence set, and whether an
// additional_bindings key was present. specNode.Kind == 0 means the spec:
// key was entirely absent from the document — a legitimate empty workspace
// (mirrors DecodeWorkspaceMetaStrict's empty-body handling).
func decodeWorkspaceEnvelopeSpec(specNode yaml.Node) (spec WorkspaceEnvelopeSpec, fieldsPresent map[string]bool, additionalBindingsDropped bool, err error) {
	fieldsPresent = map[string]bool{}
	if specNode.Kind == 0 {
		return spec, fieldsPresent, false, nil
	}

	var raw map[string]yaml.Node
	if err := specNode.Decode(&raw); err != nil {
		return spec, nil, false, fmt.Errorf("spec: %w", err)
	}

	for key := range raw {
		if !workspaceEnvelopeSpecFields[key] {
			return spec, nil, false, fmt.Errorf("spec: unknown field %q", key)
		}
	}

	if n, ok := raw["additional_bindings"]; ok && n.Kind != 0 {
		additionalBindingsDropped = true
	}
	if n, ok := raw["host_commands"]; ok {
		fieldsPresent["host_commands"] = true
		if err := n.Decode(&spec.HostCommands); err != nil {
			return spec, nil, false, fmt.Errorf("spec.host_commands: %w", err)
		}
	}
	if n, ok := raw["env"]; ok {
		fieldsPresent["env"] = true
		if err := n.Decode(&spec.Env); err != nil {
			return spec, nil, false, fmt.Errorf("spec.env: %w", err)
		}
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
	}
	if n, ok := raw["allowed_domains"]; ok {
		fieldsPresent["allowed_domains"] = true
		if err := n.Decode(&spec.AllowedDomains); err != nil {
			return spec, nil, false, fmt.Errorf("spec.allowed_domains: %w", err)
		}
	}
	if n, ok := raw["extra_repos"]; ok {
		fieldsPresent["extra_repos"] = true
		if err := n.Decode(&spec.ExtraRepos); err != nil {
			return spec, nil, false, fmt.Errorf("spec.extra_repos: %w", err)
		}
	}
	if n, ok := raw["container_image"]; ok {
		fieldsPresent["container_image"] = true
		if err := n.Decode(&spec.ContainerImage); err != nil {
			return spec, nil, false, fmt.Errorf("spec.container_image: %w", err)
		}
	}
	if n, ok := raw["init_script"]; ok {
		fieldsPresent["init_script"] = true
		// A null value (`init_script:` with nothing after it) decodes into the
		// zero string, which this schema reads as "no init.sh" — the same
		// answer as `init_script: ""`. That is the right collapse here (there
		// is no third state for a file to be in), unlike env above where nil
		// vs. empty map would otherwise reach MergeInto as two different
		// things.
		if err := n.Decode(&spec.InitScript); err != nil {
			return spec, nil, false, fmt.Errorf("spec.init_script: %w", err)
		}
	}
	if n, ok := raw["capabilities"]; ok {
		fieldsPresent["capabilities"] = true
		// PR-1d codex round-2 Major: plain yaml.Node.Decode has no
		// KnownFields option, so a typo'd nested key (e.g. "dcoker: {}")
		// would otherwise decode silently into a zero-value Capabilities
		// instead of being rejected the way DecodeWorkspaceMetaStrict's
		// top-level KnownFields(true) already rejects an unknown *outer*
		// field — see decodeStrictNode's own doc comment.
		if err := decodeStrictNode(n, &spec.Capabilities); err != nil {
			return spec, nil, false, fmt.Errorf("spec.capabilities: %w", err)
		}
	}
	if n, ok := raw["projects"]; ok {
		fieldsPresent["projects"] = true
		// Same KnownFields gap as capabilities above: without this, a
		// typo'd "nam: intended" entry silently decodes with Name == "",
		// which ResolveProjectRef then matches via its substring-match
		// fallback (Contains(x, "") is always true) against whichever
		// project happens to be registered — round-2's concrete failure.
		if err := decodeStrictNode(n, &spec.Projects); err != nil {
			return spec, nil, false, fmt.Errorf("spec.projects: %w", err)
		}
		for i, p := range spec.Projects {
			if strings.TrimSpace(p.Name) == "" {
				return spec, nil, false, fmt.Errorf("spec.projects[%d]: name is required and must be non-empty", i)
			}
		}
	}

	return spec, fieldsPresent, additionalBindingsDropped, nil
}

// decodeStrictNode decodes n into out with KnownFields(true) enforcement
// (PR-1d codex round-2 Major: "strict decoding doesn't extend to
// spec.projects[]/spec.capabilities"). n arrives here as a yaml.Node parsed
// out of decodeWorkspaceEnvelopeSpec's own raw map[string]yaml.Node — not
// through a top-level yaml.NewDecoder — so the KnownFields(true) call on
// DecodeWorkspaceEnvelopeDocuments's outer *yaml.Decoder (which only
// governs rawWorkspaceEnvelope's own fields) never reaches it, and
// yaml.Node.Decode itself has no strictness option. Re-marshaling n back to
// bytes and re-parsing through a fresh strict *yaml.Decoder is the
// documented way (gopkg.in/yaml.v3) to apply KnownFields to an
// already-parsed Node's subtree.
func decodeStrictNode(n yaml.Node, out any) error {
	raw, err := yaml.Marshal(&n)
	if err != nil {
		return fmt.Errorf("re-marshal node: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(out)
}
