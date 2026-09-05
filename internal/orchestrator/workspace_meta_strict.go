package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"gopkg.in/yaml.v3"
)

// This file provides strict (unknown-field-rejecting) YAML decoding for
// WorkspaceMeta, used by the POST /api/workspaces (create) and
// PUT /api/workspaces/{slug} (replace) handlers. Without this, a typo in a
// hand-authored workspace yaml body would silently be dropped instead of rejected.
//
// additional_bindings is the one deliberate exception to "unknown field is
// rejected": the workspace-scoped AdditionalBindings mechanism was retired,
// but it had real production usage, so workspaceMetaStrict keeps it as a
// known key (decoded into a raw yaml.Node sink, not validated) so an
// existing body still decodes without error; toWorkspaceMeta logs a warning
// and discards the value.

// workspaceMetaStrict mirrors WorkspaceMeta (workspace_meta.go) field-for-
// field. IMPORTANT: keep in sync with WorkspaceMeta.
//
// A `kits:` key in a POST/PUT/import body is deliberately NOT a known field
// here: a caller submitting one gets a loud "unknown field kits" rejection
// rather than a silent no-op. Client-side callers that still need to resolve
// a legacy kits: list do so themselves against the raw yaml and submit an
// already-materialized (kits-free) body — see
// cmd/workspace.go's ensureWorkspaceExistsForAssign.
type workspaceMetaStrict struct {
	Env            map[string]string `yaml:"env,omitempty"`
	Capabilities   Capabilities      `yaml:"capabilities,omitempty"`
	AllowedDomains []string          `yaml:"allowed_domains,omitempty"`
	ExtraRepos     []string          `yaml:"extra_repos,omitempty"`
	Services       []string          `yaml:"services,omitempty"`
	HostCommands   []string          `yaml:"host_commands,omitempty"`
	ContainerImage string            `yaml:"container_image,omitempty"`

	// AdditionalBindings is a retired-but-tolerated sink (see this file's
	// package doc comment): a yaml.Node accepts any shape (mapping, scalar
	// short form, or even a malformed one) at the `additional_bindings` key
	// without validating its contents — there is nothing left downstream
	// that would care about a nested typo, since the value is discarded
	// either way. A zero-value yaml.Node (Kind == 0) means the key was
	// absent from the decoded document; any other Kind means it was present
	// (including an explicit empty list or null), which is what
	// toWorkspaceMeta below checks to decide whether to warn.
	AdditionalBindings yaml.Node `yaml:"additional_bindings"`
}

// toWorkspaceMeta converts the strict decode-only shape to the public
// WorkspaceMeta type. additional_bindings is intentionally not carried over
// — WorkspaceMeta has no field for it any more — but a present key is
// logged so an operator finds out its content is being silently discarded.
func (s workspaceMetaStrict) toWorkspaceMeta() *WorkspaceMeta {
	if s.AdditionalBindings.Kind != 0 {
		slog.Warn("workspace meta: additional_bindings is no longer supported (retired in docs/plans/home-workspace-volume.md Phase 4 PR4); the field is parsed but ignored")
	}
	return &WorkspaceMeta{
		Env:            s.Env,
		Capabilities:   s.Capabilities,
		AllowedDomains: s.AllowedDomains,
		ExtraRepos:     s.ExtraRepos,
		Services:       s.Services,
		HostCommands:   s.HostCommands,
		ContainerImage: s.ContainerImage,
	}
}

// additionalBindingsKeyPresent reports whether raw's top-level
// additional_bindings: key is present in the parsed document, using the
// same tolerate-anything yaml.Node sink technique
// workspaceMetaStrict.AdditionalBindings uses for the wire path: any shape
// decodes without error, so a caller gets a presence answer instead of an
// unrelated parse error.
//
// Shared by readWorkspaceYAMLSnapshot (workspace_migration.go) and
// WorkspaceStore.Load's yaml-mode path (workspace_store.go), both of which
// discard the value (WorkspaceMeta has no field for it any more) but log a
// warning when present.
//
// A zero-value yaml.Node (Kind == 0) means the key was absent — the same
// convention workspaceMetaStrict.AdditionalBindings documents above.
func additionalBindingsKeyPresent(raw []byte) (bool, error) {
	var sink struct {
		AdditionalBindings yaml.Node `yaml:"additional_bindings"`
	}
	if err := yaml.Unmarshal(raw, &sink); err != nil {
		return false, err
	}
	return sink.AdditionalBindings.Kind != 0, nil
}

// RejectTrailingYAMLDocument guards against a silent multi-document drop:
// yaml.Decoder.Decode only ever consumes a single "---"-delimited document
// per call and silently ignores everything after it, so a hand-authored
// multi-document workspace yaml would have its second document silently
// dropped with no error. This calls dec on the same Decoder immediately
// after the real decode succeeded, expecting io.EOF (nothing left); any
// other outcome is reported as an error rather than silently discarded.
//
// Exported so cmd/workspace.go's extractLegacyWorkspaceKitRefs can reuse the
// exact same trailing-document check on raw local workspace.yaml bytes.
func RejectTrailingYAMLDocument(dec *yaml.Decoder) error {
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
// too — see RejectTrailingYAMLDocument.
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
		if envErr := envelopeSentToTheWrongDoor(data); envErr != nil {
			return nil, envErr
		}
		return nil, fmt.Errorf("decode workspace meta: %w", err)
	}
	if err := RejectTrailingYAMLDocument(dec); err != nil {
		return nil, fmt.Errorf("decode workspace meta: %w", err)
	}
	return strict.toWorkspaceMeta(), nil
}

// envelopeSentToTheWrongDoor returns the error to report when data is an
// apiVersion/kind envelope — the shape `boid workspace export` writes and
// `boid workspace apply` reads — that arrived at one of the decoders above,
// which take a bare meta mapping instead. It returns nil for anything else,
// leaving the caller's own decode error to stand: the raw KnownFields error
// otherwise names an unexported Go type and says nothing about the
// one-word-different command that does accept the document.
//
// Detection is deliberately loose — ANY apiVersion or kind key, not just the
// supported values. A document carrying `apiVersion: boid.dev/v2` from a
// newer boid, or a typo'd kind, is still unmistakably an envelope, and its
// author is still better served by being sent to `apply` (whose own
// validation will then say exactly what is wrong with it) than by a list of
// unknown fields from a decoder that was never going to accept the document.
func envelopeSentToTheWrongDoor(data []byte) error {
	var probe struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	// Loose (no KnownFields): every other key in the document is expected to
	// be unknown to this probe, which is the whole point of it.
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}
	if probe.APIVersion == "" && probe.Kind == "" {
		return nil
	}
	return fmt.Errorf(
		"this is a %s %s envelope document (what `boid workspace export` writes), which create/edit do not take — they want a bare workspace meta mapping. Apply it instead: boid workspace apply -f <file>",
		orEmpty(probe.APIVersion, "?"), orEmpty(probe.Kind, "?"))
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// workspaceCreateStrict is the on-wire shape of a POST /api/workspaces
// create body: the target slug inlined alongside the same fields
// workspaceMetaStrict decodes. The embedded workspaceMetaStrict is decoded
// in the very same dec.Decode call as Slug (yaml.v3's `inline` tag folds its
// fields into the outer struct during the same decode pass), so
// KnownFields(true) enforcement covers the combined field set exactly as it
// does for workspaceMetaStrict alone.
type workspaceCreateStrict struct {
	Slug                string `yaml:"slug"`
	workspaceMetaStrict `yaml:",inline"`
}

// DecodeWorkspaceCreateStrict strictly decodes a POST /api/workspaces create
// body into its target slug and WorkspaceMeta. An empty body decodes to an
// empty slug (the caller is expected to reject that with "slug is required")
// and a zero-value WorkspaceMeta, mirroring DecodeWorkspaceMetaStrict's
// empty-body handling. A second "---"-delimited document is rejected too —
// see RejectTrailingYAMLDocument.
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
		if envErr := envelopeSentToTheWrongDoor(data); envErr != nil {
			return "", nil, envErr
		}
		return "", nil, fmt.Errorf("decode workspace create body: %w", err)
	}
	if err := RejectTrailingYAMLDocument(dec); err != nil {
		return "", nil, fmt.Errorf("decode workspace create body: %w", err)
	}
	return strict.Slug, strict.workspaceMetaStrict.toWorkspaceMeta(), nil
}
