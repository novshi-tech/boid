package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"io"

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
type WorkspaceEnvelopeSpec struct {
	HostCommands   []string                   `yaml:"host_commands,omitempty"`
	Env            map[string]string          `yaml:"env,omitempty"`
	AllowedDomains []string                   `yaml:"allowed_domains,omitempty"`
	ExtraRepos     []string                   `yaml:"extra_repos,omitempty"`
	ContainerImage string                     `yaml:"container_image,omitempty"`
	Capabilities   Capabilities               `yaml:"capabilities,omitempty"`
	Projects       []WorkspaceEnvelopeProject `yaml:"projects,omitempty"`
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
		if err := n.Decode(&spec.Capabilities); err != nil {
			return spec, nil, false, fmt.Errorf("spec.capabilities: %w", err)
		}
	}
	if n, ok := raw["projects"]; ok {
		fieldsPresent["projects"] = true
		if err := n.Decode(&spec.Projects); err != nil {
			return spec, nil, false, fmt.Errorf("spec.projects: %w", err)
		}
	}

	return spec, fieldsPresent, additionalBindingsDropped, nil
}
