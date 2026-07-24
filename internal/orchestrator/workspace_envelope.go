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
	// "projects"). A key absent from this map was not present in the
	// document at all and MergeInto leaves the corresponding WorkspaceMeta
	// field untouched; a key present (even if its decoded value is the zero
	// value / an explicit empty list or map) means the document explicitly
	// wants that field replaced with the decoded value.
	FieldsPresent map[string]bool
	// AdditionalBindingsDropped is true when the source document had a
	// spec.additional_bindings key (any shape, including an explicit empty
	// list or null) — retired per docs/plans/volume-only-daemon.md §論点g;
	// the value is always discarded. Callers should log/print a warning
	// rather than fail the apply.
	AdditionalBindingsDropped bool
}

// NewWorkspaceEnvelopeFromMeta builds a WorkspaceEnvelope for `boid workspace
// export` from a workspace's current WorkspaceMeta and its resolved project
// entries (name + url, url may be empty pre-PR-2 — see
// WorkspaceEnvelopeProject's doc comment). meta may be nil (an empty/never-
// configured workspace), in which case the spec carries only projects (if
// any).
func NewWorkspaceEnvelopeFromMeta(name string, meta *WorkspaceMeta, projects []WorkspaceEnvelopeProject) *WorkspaceEnvelope {
	spec := WorkspaceEnvelopeSpec{Projects: projects}
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
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		docs = append(docs, doc)
	}
	return docs, nil
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
