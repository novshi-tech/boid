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
// PUT /api/workspaces/{slug} (replace) handlers (internal/api/workspace.go,
// docs/plans/workspace-db-consolidation.md PR4 Step C/E). Without this, a
// typo in a hand-authored workspace yaml body (e.g. "hostcommands" instead
// of "host_commands") would silently be dropped instead of rejected — the
// same class of bug PR2's hostCommandSpecStrict (host_commands_config.go)
// exists to prevent for the aggregated host_commands.yaml config.
//
// additional_bindings is the one deliberate exception to "unknown field is
// rejected" (docs/plans/home-workspace-volume.md Phase 4 PR4): the
// workspace-scoped AdditionalBindings mechanism was retired outright (see
// WorkspaceMeta's own doc comment), but unlike `kits:` — which Phase 2.5 PR7
// judged fully dead and now rejects outright — additional_bindings had real
// production usage, so a hard "unknown field" break on every existing
// workspace yaml/PUT body that still carries it would be a needlessly
// disruptive way to retire it. workspaceMetaStrict below keeps
// additional_bindings as a known key (decoded into a raw yaml.Node sink,
// not validated or converted to a []BindMount any more) so such a body still
// decodes without error; toWorkspaceMeta logs a warning and discards the
// value instead of mapping it onto WorkspaceMeta, which has no field for it.

// workspaceMetaStrict mirrors WorkspaceMeta (workspace_meta.go) field-for-
// field. IMPORTANT: keep in sync with WorkspaceMeta.
//
// A `kits:` key in a POST/PUT/import body is deliberately NOT a known field
// here any more (Phase 2.5 PR7, decision 12: no fallback on the wire): a
// caller submitting one now gets a loud "unknown field kits" rejection
// rather than a silent no-op. The two client-side callers that still need to
// resolve a legacy kits: list (cmd/workspace.go's ensureWorkspaceExistsForAssign,
// for the `boid workspace assign` auto-create convenience path hand-authored
// / e2e-fixture shadow yaml files rely on) do so themselves, against the raw
// yaml, and submit an already-materialized (kits-free) body — see that
// function's doc comment.
type workspaceMetaStrict struct {
	Env            map[string]string `yaml:"env,omitempty"`
	Capabilities   Capabilities      `yaml:"capabilities,omitempty"`
	AllowedDomains []string          `yaml:"allowed_domains,omitempty"`
	ExtraRepos     []string          `yaml:"extra_repos,omitempty"`
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
// WorkspaceMeta type. additional_bindings is intentionally NOT carried over
// — WorkspaceMeta has no field for it any more (Phase 4 PR4) — but a present
// key is logged so an operator submitting a stale workspace yaml/PUT body
// finds out its additional_bindings content is being silently discarded
// rather than applied.
func (s workspaceMetaStrict) toWorkspaceMeta() *WorkspaceMeta {
	if s.AdditionalBindings.Kind != 0 {
		slog.Warn("workspace meta: additional_bindings is no longer supported (retired in docs/plans/home-workspace-volume.md Phase 4 PR4); the field is parsed but ignored")
	}
	return &WorkspaceMeta{
		Env:            s.Env,
		Capabilities:   s.Capabilities,
		AllowedDomains: s.AllowedDomains,
		ExtraRepos:     s.ExtraRepos,
		HostCommands:   s.HostCommands,
		ContainerImage: s.ContainerImage,
	}
}

// additionalBindingsKeyPresent (Codex Should-fix, PR4 review,
// docs/plans/home-workspace-volume.md) reports whether raw's top-level
// additional_bindings: key is present in the parsed document, using the same
// tolerate-anything yaml.Node sink technique workspaceMetaStrict.AdditionalBindings
// above uses for the wire (POST/PUT) path: any shape — a well-formed list, a
// malformed nested structure, an explicit null, or an empty list — decodes
// without error, so a caller whose additional_bindings section would fail a
// strict []BindMount decode still gets a presence answer instead of an
// unrelated parse error (there is nothing left downstream to validate the
// value against; it is discarded either way).
//
// Shared by readWorkspaceYAMLSnapshot (workspace_migration.go) and
// WorkspaceStore.Load's yaml-mode path (workspace_store.go): both discard
// the value (WorkspaceMeta has no field for it any more) but, unlike the
// wire path above, previously did so with no warning at all — the plan
// (docs/plans/home-workspace-volume.md) requires "parse continues + ignore +
// warn" for every legacy read path, not only the wire one.
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

// RejectTrailingYAMLDocument guards against MINOR 2 (codex review round 2,
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
//
// Exported (MAJOR 3, codex review round 1) so cmd/workspace.go's
// extractLegacyWorkspaceKitRefs can reuse the exact same trailing-document
// check on the raw local workspace.yaml bytes it reads directly, instead of
// re-implementing it — see that function's doc comment for why a plain
// yaml.Unmarshal-into-map there would otherwise defeat this same guard.
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
// too (MINOR 2, codex review round 2) — see RejectTrailingYAMLDocument.
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
	if err := RejectTrailingYAMLDocument(dec); err != nil {
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
// (MINOR 2, codex review round 2) — see RejectTrailingYAMLDocument.
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
	if err := RejectTrailingYAMLDocument(dec); err != nil {
		return "", nil, fmt.Errorf("decode workspace create body: %w", err)
	}
	return strict.Slug, strict.workspaceMetaStrict.toWorkspaceMeta(), nil
}
