package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// This file provides strict (unknown-field-rejecting) YAML decoding for
// WorkspaceMeta, used by the POST /api/workspaces (create) and
// PUT /api/workspaces/{slug} (replace) handlers (internal/api/workspace.go,
// docs/plans/workspace-db-consolidation.md PR4 Step C/E). Without this, a
// typo in a hand-authored workspace yaml body (e.g. "hostcommands" instead
// of "host_commands") would silently be dropped instead of rejected — the
// same class of bug PR2's hostCommandSpecStrict (host_commands_config.go)
// exists to prevent for the aggregated host_commands.yaml config.
//
// bindMountStrict exists for the same reason hostCommandSpecStrict exists:
// BindMount has its own custom UnmarshalYAML (to accept the short-form
// scalar convenience, e.g. `additional_bindings: [/opt/tool]`), and per
// gopkg.in/yaml.v3, a Node.Decode call inside a custom UnmarshalYAML always
// starts a fresh decode with default (non-strict) settings — regardless of
// the KnownFields(true) Decoder that produced the node. Decoding
// additional_bindings through the public BindMount type would therefore
// silently drop a nested typo (e.g. "modee: rw"). bindMountStrict has the
// identical field layout plus its own UnmarshalYAML (MINOR 3-a, codex
// review: the public additional_bindings contract allows the same short-form
// scalar BindMount does, which a plain struct-tag decode cannot accept) —
// see that method's doc comment for how it keeps the strict guarantee
// despite needing a custom UnmarshalYAML of its own.

// bindMountStrict mirrors BindMount (spec_types.go) field-for-field.
// IMPORTANT: keep this in sync with BindMount — a field added there must be
// mirrored here (both in the struct and in bindMountStrictKnownFields
// below), or the strict decode will silently ignore it (defeating the point
// of this type).
type bindMountStrict struct {
	Source   string `yaml:"source"`
	Target   string `yaml:"target,omitempty"`
	Mode     string `yaml:"mode"`
	IsFile   bool   `yaml:"is_file,omitempty"`
	Optional bool   `yaml:"optional,omitempty"`
}

// bindMountStrictKnownFields lists every yaml key bindMountStrict's mapping
// form accepts, used by UnmarshalYAML below to manually enforce strictness
// (see that method's doc comment for why KnownFields(true) alone cannot do
// this here).
var bindMountStrictKnownFields = map[string]bool{
	"source": true, "target": true, "mode": true, "is_file": true, "optional": true,
}

// UnmarshalYAML accepts the same two equivalent forms BindMount.UnmarshalYAML
// does (MINOR 3-a, codex review, docs/plans/workspace-db-consolidation.md):
//
//	additional_bindings:
//	  - /host/path              # short form: equivalent to {source: "/host/path"}
//	  - source: /host/path      # struct form
//	    mode: rw
//
// Before this method existed, bindMountStrict deliberately had *no* custom
// UnmarshalYAML at all — see this file's package doc comment: yaml.v3's
// Node.Decode inside a custom UnmarshalYAML always runs with default
// (non-strict) settings regardless of the Decoder that produced the node,
// so naively delegating to a type-aliased Decode here (the way
// BindMount.UnmarshalYAML does) would silently drop a typo'd nested field
// even under KnownFields(true) — defeating the entire point of this strict
// type. This method keeps the strict guarantee by checking the mapping
// form's keys against bindMountStrictKnownFields by hand *before* decoding,
// rather than delegating that check to the (non-strict-in-this-context)
// decoder.
func (b *bindMountStrict) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		b.Source = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("additional_bindings: expected a string or a mapping, got %v", node.Kind)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !bindMountStrictKnownFields[key] {
			return fmt.Errorf("additional_bindings: unknown field %q at line %d", key, node.Content[i].Line)
		}
	}
	type bindMountStrictAlias bindMountStrict
	var aux bindMountStrictAlias
	if err := node.Decode(&aux); err != nil {
		return err
	}
	*b = bindMountStrict(aux)
	return nil
}

// toBindMount converts the strict decode-only shape to the public BindMount
// type. The two structs share the same field layout (name, type, and order),
// so a plain Go type conversion is valid — struct tags are not part of Go's
// convertibility check.
func (b bindMountStrict) toBindMount() BindMount {
	return BindMount(b)
}

// workspaceMetaStrict mirrors WorkspaceMeta (workspace_meta.go) field-for-
// field, replacing AdditionalBindings' element type with bindMountStrict so
// the nested strict-decode guarantee holds (see this file's package doc
// comment). IMPORTANT: keep in sync with WorkspaceMeta.
type workspaceMetaStrict struct {
	Kits               []string          `yaml:"kits,omitempty"`
	Env                map[string]string `yaml:"env,omitempty"`
	Capabilities       Capabilities      `yaml:"capabilities,omitempty"`
	AllowedDomains     []string          `yaml:"allowed_domains,omitempty"`
	ExtraRepos         []string          `yaml:"extra_repos,omitempty"`
	HostCommands       []string          `yaml:"host_commands,omitempty"`
	ContainerImage     string            `yaml:"container_image,omitempty"`
	AdditionalBindings []bindMountStrict `yaml:"additional_bindings,omitempty"`
}

// toWorkspaceMeta converts the strict decode-only shape to the public
// WorkspaceMeta type, converting each AdditionalBindings entry individually
// (a slice-of-different-named-struct conversion cannot be done with a single
// Go type conversion the way the scalar/nested-struct cases above can).
func (s workspaceMetaStrict) toWorkspaceMeta() *WorkspaceMeta {
	var bindings []BindMount
	if len(s.AdditionalBindings) > 0 {
		bindings = make([]BindMount, len(s.AdditionalBindings))
		for i, b := range s.AdditionalBindings {
			bindings[i] = b.toBindMount()
		}
	}
	return &WorkspaceMeta{
		Kits:               s.Kits,
		Env:                s.Env,
		Capabilities:       s.Capabilities,
		AllowedDomains:     s.AllowedDomains,
		ExtraRepos:         s.ExtraRepos,
		HostCommands:       s.HostCommands,
		ContainerImage:     s.ContainerImage,
		AdditionalBindings: bindings,
	}
}

// rejectTrailingYAMLDocument guards against MINOR 2 (codex review round 2,
// docs/plans/workspace-db-consolidation.md): yaml.Decoder.Decode only ever
// consumes a single "---"-delimited document per call and silently ignores
// everything after it — a caller who hand-authors a multi-document workspace
// yaml (e.g. by copy-pasting a second workspace's fields below a `---`
// separator, perhaps intending some other tool to consume it) would have the
// second document silently dropped with no error, and only the first
// document's fields would ever reach the DB. This calls dec on the same
// Decoder immediately after the real decode succeeded, expecting io.EOF
// (nothing left); any other outcome — a second document decoding cleanly, or
// even a malformed one — is reported as an error rather than silently
// discarded.
func rejectTrailingYAMLDocument(dec *yaml.Decoder) error {
	var trailing yaml.Node
	err := dec.Decode(&trailing)
	if err == nil {
		return errors.New("multiple YAML documents are not supported (found content after the first '---'-delimited document)")
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("checking for trailing YAML documents: %w", err)
	}
	return nil
}

// DecodeWorkspaceMetaStrict strictly decodes a workspace meta YAML document
// (as used by PUT /api/workspaces/{slug} — the whole-document replace body).
// An empty/whitespace-only body decodes to a zero-value WorkspaceMeta rather
// than erroring on io.EOF: a body-less create/edit is a legitimate way to
// declare an empty workspace. A typo'd or unknown field (top-level or
// nested inside additional_bindings/capabilities) is rejected with an error
// naming the offending field. A second "---"-delimited document is rejected
// too (MINOR 2, codex review round 2) — see rejectTrailingYAMLDocument.
func DecodeWorkspaceMetaStrict(data []byte) (*WorkspaceMeta, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &WorkspaceMeta{}, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var strict workspaceMetaStrict
	if err := dec.Decode(&strict); err != nil {
		if errors.Is(err, io.EOF) {
			return &WorkspaceMeta{}, nil
		}
		return nil, fmt.Errorf("decode workspace meta: %w", err)
	}
	if err := rejectTrailingYAMLDocument(dec); err != nil {
		return nil, fmt.Errorf("decode workspace meta: %w", err)
	}
	return strict.toWorkspaceMeta(), nil
}

// workspaceCreateStrict is the on-wire shape of a POST /api/workspaces
// create body: the target slug inlined alongside the same fields
// workspaceMetaStrict decodes (docs/plans/workspace-db-consolidation.md
// Step C — "slug は body 内"). The embedded workspaceMetaStrict is decoded
// in the very same dec.Decode call as Slug (yaml.v3's `inline` tag folds its
// fields into the outer struct during the same decode pass, not via a
// separate nested Decode call), so KnownFields(true) enforcement covers the
// combined field set exactly as it does for workspaceMetaStrict alone.
type workspaceCreateStrict struct {
	Slug                string `yaml:"slug"`
	workspaceMetaStrict `yaml:",inline"`
}

// DecodeWorkspaceCreateStrict strictly decodes a POST /api/workspaces create
// body into its target slug and WorkspaceMeta. An empty body decodes to an
// empty slug (the caller is expected to reject that with "slug is required")
// and a zero-value WorkspaceMeta, mirroring DecodeWorkspaceMetaStrict's
// empty-body handling. A second "---"-delimited document is rejected too
// (MINOR 2, codex review round 2) — see rejectTrailingYAMLDocument.
func DecodeWorkspaceCreateStrict(data []byte) (slug string, meta *WorkspaceMeta, err error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return "", &WorkspaceMeta{}, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var strict workspaceCreateStrict
	if err := dec.Decode(&strict); err != nil {
		if errors.Is(err, io.EOF) {
			return "", &WorkspaceMeta{}, nil
		}
		return "", nil, fmt.Errorf("decode workspace create body: %w", err)
	}
	if err := rejectTrailingYAMLDocument(dec); err != nil {
		return "", nil, fmt.Errorf("decode workspace create body: %w", err)
	}
	return strict.Slug, strict.workspaceMetaStrict.toWorkspaceMeta(), nil
}
