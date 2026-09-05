package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file implements the K8s-like workspace export/apply document shape: a
// single self-describing YAML document (or several, "---"-separated) that
// carries a workspace's WorkspaceMeta fields plus the project names/URLs
// assigned to it. `boid workspace export`/`apply` are the only callers; this
// is additive to, not a replacement for, the existing revision/ETag-based
// WorkspaceMeta wire format that single-resource POST/PUT bodies still use.

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
// type's doc comment) plus Projects, which has no WorkspaceMeta equivalent —
// project-workspace assignment is separate bookkeeping, not part of the
// workspace's own meta row.
//
// additional_bindings is deliberately NOT a field here (retired field);
// decodeWorkspaceEnvelopeSpec tolerates it in a source document, parsing and
// discarding it with AdditionalBindingsDropped set for the caller to warn on.
//
// None of spec's fields carry `omitempty`: missing vs. explicitly-empty is a
// load-bearing distinction (WorkspaceEnvelopeApply's FieldsPresent-driven
// merge) — omitempty would make "field is empty" indistinguishable from
// "field absent, leave untouched", breaking export→apply round trips that
// are meant to clear a field.
type WorkspaceEnvelopeSpec struct {
	HostCommands   []string          `yaml:"host_commands"`
	Env            map[string]string `yaml:"env"`
	AllowedDomains []string          `yaml:"allowed_domains"`
	ExtraRepos     []string          `yaml:"extra_repos"`
	// Services is the workspace-scoped API gateway service allowlist.
	Services       []string                   `yaml:"services"`
	ContainerImage string                     `yaml:"container_image"`
	Capabilities   Capabilities               `yaml:"capabilities"`
	Projects       []WorkspaceEnvelopeProject `yaml:"projects"`
	// TaskBehaviors / BaseBranch / ForkPoint / DefaultTaskBehavior are the
	// workspace's default project definition — see WorkspaceMeta's
	// identically-named fields for the semantics.
	TaskBehaviors       map[string]TaskBehavior `yaml:"task_behaviors"`
	BaseBranch          string                  `yaml:"base_branch"`
	ForkPoint           string                  `yaml:"fork_point"`
	DefaultTaskBehavior string                  `yaml:"default_task_behavior"`
	// InitScript is the workspace's init.sh, verbatim. Unlike other fields it
	// has no WorkspaceMeta counterpart — init.sh is a file the daemon owns,
	// not a workspaces-table column — so MergeInto does not touch it;
	// ProjectAppService.ApplyWorkspace reads FieldsPresent["init_script"]
	// directly to decide whether to write/clear the file. An empty script
	// and no script collapse to the same state: both are a no-op run.
	InitScript string `yaml:"init_script"`
}

// WorkspaceEnvelopeProject is one spec.projects[] entry. URL is currently
// informational only: apply does not register a project from URL, it only
// attaches an already-registered project (matched by name) to the workspace.
type WorkspaceEnvelopeProject struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url,omitempty"`
}

// workspaceEnvelopeSpecFields is the allow-list of spec.* keys a document
// may carry. additional_bindings is listed so it is tolerated (parsed into
// AdditionalBindingsDropped, then discarded) rather than rejected outright.
// Anything else is rejected with "unknown field" (typo guard).
var workspaceEnvelopeSpecFields = map[string]bool{
	"host_commands":         true,
	"env":                   true,
	"allowed_domains":       true,
	"extra_repos":           true,
	"services":              true,
	"container_image":       true,
	"capabilities":          true,
	"projects":              true,
	"init_script":           true,
	"additional_bindings":   true,
	"task_behaviors":        true,
	"base_branch":           true,
	"fork_point":            true,
	"default_task_behavior": true,
}

// WorkspaceEnvelopeApply is the result of decoding one Workspace document
// for `boid workspace apply`: the parsed envelope plus enough bookkeeping
// for the K8s-style "missing field = don't touch, present-but-empty = clear"
// merge semantics that a plain *WorkspaceEnvelope alone cannot represent — a
// zero-value Go slice/map cannot be told apart from "the key was absent"
// once decoded into a plain struct.
type WorkspaceEnvelopeApply struct {
	Envelope *WorkspaceEnvelope
	// FieldsPresent records which spec.* keys were present in the source
	// document, by yaml key name. A key absent from this map was not present
	// in the document at all and MergeInto leaves the corresponding
	// WorkspaceMeta field untouched; a key present (even with a zero-value
	// decode) means the document wants that field replaced.
	//
	// "init_script" is in this map but not in MergeInto's switch — its
	// consumer is ProjectAppService.ApplyWorkspace, which reads the same
	// flag directly to decide whether to write/clear the file.
	FieldsPresent map[string]bool
	// AdditionalBindingsDropped is true when the source document had a
	// spec.additional_bindings key (any shape) — the value is always
	// discarded; callers should log/print a warning rather than fail the apply.
	AdditionalBindingsDropped bool
}

// NewWorkspaceEnvelopeFromMeta builds a WorkspaceEnvelope for `boid workspace
// export` from a workspace's current WorkspaceMeta, its resolved project
// entries, and its init.sh. meta may be nil (an empty/never-configured
// workspace), in which case the spec carries only projects (if any) and the
// script. initScript "" is emitted as `init_script: ""`, read back as an
// explicit clear — see WorkspaceEnvelopeSpec.InitScript.
func NewWorkspaceEnvelopeFromMeta(name string, meta *WorkspaceMeta, projects []WorkspaceEnvelopeProject, initScript string) *WorkspaceEnvelope {
	spec := WorkspaceEnvelopeSpec{Projects: projects, InitScript: initScript}
	if meta != nil {
		spec.HostCommands = meta.HostCommands
		spec.Env = meta.Env
		spec.AllowedDomains = meta.AllowedDomains
		spec.ExtraRepos = meta.ExtraRepos
		spec.Services = meta.Services
		spec.ContainerImage = meta.ContainerImage
		spec.Capabilities = meta.Capabilities
		spec.TaskBehaviors = meta.TaskBehaviors
		spec.BaseBranch = meta.BaseBranch
		spec.ForkPoint = meta.ForkPoint
		spec.DefaultTaskBehavior = meta.DefaultTaskBehavior
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
// spec.init_script is deliberately absent from the field list below: it has
// no *WorkspaceMeta column to merge into. ProjectAppService.ApplyWorkspace
// honors its FieldsPresent flag separately, outside this function's DB
// transaction (a file write cannot be made atomic with it anyway).
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
	if a.FieldsPresent["services"] {
		merged.Services = spec.Services
	}
	if a.FieldsPresent["container_image"] {
		merged.ContainerImage = spec.ContainerImage
	}
	if a.FieldsPresent["capabilities"] {
		merged.Capabilities = spec.Capabilities
	}
	if a.FieldsPresent["task_behaviors"] {
		merged.TaskBehaviors = spec.TaskBehaviors
	}
	if a.FieldsPresent["base_branch"] {
		merged.BaseBranch = spec.BaseBranch
	}
	if a.FieldsPresent["fork_point"] {
		merged.ForkPoint = spec.ForkPoint
	}
	if a.FieldsPresent["default_task_behavior"] {
		merged.DefaultTaskBehavior = spec.DefaultTaskBehavior
	}
	return merged
}

// DecodeWorkspaceEnvelopeDocuments decodes data as one or more "---"-
// separated Workspace envelope documents. An empty/whitespace-only body is
// an error — an apply file with nothing in it is almost certainly a
// mistake, not a legitimate no-op apply. Every document must independently satisfy:
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
	// Reject an unknown top-level field (rawWorkspaceEnvelope's own struct
	// fields only) instead of silently dropping it; spec's own allow-list is
	// enforced separately by decodeWorkspaceEnvelopeSpec below.
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

// bareMetaKnownFieldNames is every yaml key bareMetaSentToTheWrongDoor
// treats as evidence of a bare workspace-meta document: the yaml tag of
// every WorkspaceMeta field, plus "slug" and "additional_bindings" which
// WorkspaceMeta itself has no field for but a real bare-meta document can
// still legitimately carry.
//
// This is a package-level var, not inlined, so
// TestBareMetaKnownFieldNames_CoversEveryWorkspaceMetaField can reflect over
// WorkspaceMeta's real yaml tags and assert every one is a key here — a
// future WorkspaceMeta field not added here would silently narrow this
// guard's detection.
//
// Known gap: task_behaviors/base_branch/fork_point/default_task_behavior
// are listed here (the drift test forces it) but workspaceMetaStrict does
// not accept them yet — a bare document using only one of them still falls
// through to create/edit's own "unknown field" rejection instead of a
// pointer to `boid workspace apply`.
var bareMetaKnownFieldNames = map[string]bool{
	"slug":                  true,
	"env":                   true,
	"capabilities":          true,
	"allowed_domains":       true,
	"extra_repos":           true,
	"services":              true,
	"host_commands":         true,
	"container_image":       true,
	"additional_bindings":   true,
	"task_behaviors":        true,
	"base_branch":           true,
	"fork_point":            true,
	"default_task_behavior": true,
}

// bareMetaSentToTheWrongDoor is the reciprocal of workspace_meta_strict.go's
// envelopeSentToTheWrongDoor: it detects a bare workspace-meta document (no
// apiVersion/kind, host_commands:/env:/... at the top level — what `boid
// workspace create --from-file`/`edit --from-file` take) landing at
// `apply`'s decoder instead, and turns the resulting raw KnownFields
// unmarshal error (an unexported Go type name) into a message pointing at
// the command that actually accepts this shape.
//
// Detection is deliberately loose: apiVersion and kind both absent, AND at
// least one field name WorkspaceMeta actually has is present at the top
// level. A document matching neither falls through to the original decode error.
func bareMetaSentToTheWrongDoor(data []byte) error {
	// Decoded as a generic map: bareMetaKnownFieldNames is the single source
	// of truth for which keys count as "looks like a meta document". Only
	// the first "---" document is read; a multi-document file whose later
	// document is the malformed one falls through to the original error.
	var doc map[string]yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	if v, ok := doc["apiVersion"]; ok && v.Value != "" {
		return nil
	}
	if v, ok := doc["kind"]; ok && v.Value != "" {
		return nil
	}
	foundMetaField := false
	for key := range doc {
		if bareMetaKnownFieldNames[key] {
			foundMetaField = true
			break
		}
	}
	if !foundMetaField {
		return nil
	}
	return errors.New(
		"this looks like a bare workspace meta document (no apiVersion/kind), which `apply` does not take — " +
			"it wants a boid.dev/v1 Workspace envelope (what `boid workspace export` writes). Use " +
			"`boid workspace create <slug> --from-file <file>` for a brand-new workspace, or " +
			"`boid workspace edit <slug> --from-file <file>` for an existing one, instead")
}

// SplitWorkspaceEnvelopeDocuments splits data (one or more "---"-separated
// yaml documents) into the raw bytes of each individual document, without
// going through WorkspaceEnvelope's Go struct representation. `boid
// workspace apply` uses this to forward each document's original
// field-presence exactly as written, one request per document: re-marshaling
// the decoded struct would lose the missing-vs-empty distinction, since a
// zero-value Go field marshals the same whether or not the source had that
// key at all. Round-tripping through yaml.Node avoids that: an absent key
// has no corresponding child node, so re-marshaling reproduces the source's
// exact key set.
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
		// own reader can take back — yaml.v3 cannot re-emit some preserved
		// source styles correctly. See forceRoundTrippableScalars.
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
	if n, ok := raw["services"]; ok {
		fieldsPresent["services"] = true
		if err := n.Decode(&spec.Services); err != nil {
			return spec, nil, false, fmt.Errorf("spec.services: %w", err)
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
		// plain yaml.Node.Decode has no KnownFields option, so use
		// decodeStrictNode to reject a typo'd nested key instead of
		// silently decoding into a zero-value Capabilities.
		if err := decodeStrictNode(n, &spec.Capabilities); err != nil {
			return spec, nil, false, fmt.Errorf("spec.capabilities: %w", err)
		}
	}
	if n, ok := raw["projects"]; ok {
		fieldsPresent["projects"] = true
		// Same KnownFields gap as capabilities above: without this, a
		// typo'd "nam: intended" entry silently decodes with Name == "",
		// which ResolveProjectRef's substring-match fallback then matches
		// against whichever project happens to be registered.
		if err := decodeStrictNode(n, &spec.Projects); err != nil {
			return spec, nil, false, fmt.Errorf("spec.projects: %w", err)
		}
		for i, p := range spec.Projects {
			if strings.TrimSpace(p.Name) == "" {
				return spec, nil, false, fmt.Errorf("spec.projects[%d]: name is required and must be non-empty", i)
			}
		}
	}
	if n, ok := raw["task_behaviors"]; ok {
		fieldsPresent["task_behaviors"] = true
		// Same KnownFields gap as capabilities/projects above: a typo'd
		// nested key (e.g. "raedonly: true") would otherwise decode
		// silently instead of being rejected.
		if err := decodeStrictNode(n, &spec.TaskBehaviors); err != nil {
			return spec, nil, false, fmt.Errorf("spec.task_behaviors: %w", err)
		}
		// Same validation pipeline project.yaml's own task_behaviors go
		// through: reject a malformed hook shape at decode time.
		normalized, err := validateWorkspaceDefaultTaskBehaviors("spec.task_behaviors", spec.TaskBehaviors)
		if err != nil {
			return spec, nil, false, err
		}
		spec.TaskBehaviors = normalized
	}
	if n, ok := raw["base_branch"]; ok {
		fieldsPresent["base_branch"] = true
		if err := n.Decode(&spec.BaseBranch); err != nil {
			return spec, nil, false, fmt.Errorf("spec.base_branch: %w", err)
		}
	}
	if n, ok := raw["fork_point"]; ok {
		fieldsPresent["fork_point"] = true
		if err := n.Decode(&spec.ForkPoint); err != nil {
			return spec, nil, false, fmt.Errorf("spec.fork_point: %w", err)
		}
	}
	if n, ok := raw["default_task_behavior"]; ok {
		fieldsPresent["default_task_behavior"] = true
		if err := n.Decode(&spec.DefaultTaskBehavior); err != nil {
			return spec, nil, false, fmt.Errorf("spec.default_task_behavior: %w", err)
		}
	}

	return spec, fieldsPresent, additionalBindingsDropped, nil
}

// decodeStrictNode decodes n into out with KnownFields(true) enforcement.
// n is a yaml.Node parsed out of a raw map[string]yaml.Node, so it never
// goes through the outer *yaml.Decoder's KnownFields setting, and
// yaml.Node.Decode itself has no strictness option — re-marshaling n and
// re-parsing through a fresh strict decoder is the documented way
// (gopkg.in/yaml.v3) to apply KnownFields to an already-parsed subtree.
func decodeStrictNode(n yaml.Node, out any) error {
	raw, err := yaml.Marshal(&n)
	if err != nil {
		return fmt.Errorf("re-marshal node: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(out)
}
