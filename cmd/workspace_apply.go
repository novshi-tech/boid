package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"sort"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	// workspaceApplyFile is the required -f/--file flag value for `workspace
	// apply`: a yaml file with one or more "---"-separated Workspace
	// documents (docs/plans/volume-only-daemon.md §論点g).
	workspaceApplyFile string
	// workspaceApplyDryRun previews the changes `apply` would make without
	// writing anything.
	workspaceApplyDryRun bool
)

var workspaceApplyCmd = &cobra.Command{
	Use:   "apply -f <file>",
	Short: "Apply workspace + project-assignment definitions from a boid.dev/v1 Workspace yaml file (upsert)",
	Long: `Apply one or more "apiVersion: boid.dev/v1 / kind: Workspace" yaml documents
(as produced by "boid workspace export") against the daemon: a workspace
named in the file that does not yet exist is created; one that already
exists is updated field-by-field (a spec field absent from the document
leaves the workspace's current value untouched; an explicit empty value —
e.g. "env: {}" — clears it). spec.projects[], when present, replaces the
workspace's ENTIRE project assignment set: an already-registered project
named in the list is attached, and any project currently assigned to the
workspace but absent from the list is detached (moved to the default
workspace). A projects[] entry that does not resolve to a registered
project prints a warning and apply continues (see the plan doc's §論点g for
why registration-by-URL is not yet wired — that lands in PR-2).

Each document is applied atomically (its metadata and project assignment
changes commit or roll back together, in one daemon-side transaction) — but
across documents in a multi-document file, apply stops at the first failing
document, leaving any already-applied document committed.

--dry-run runs the exact same daemon-side validation and prints what would
change, without writing anything.`,
	Args: cobra.NoArgs,
	RunE: runWorkspaceApply,
}

func init() {
	workspaceApplyCmd.Flags().StringVarP(&workspaceApplyFile, "file", "f", "", "yaml file with one or more '---'-separated Workspace documents (required)")
	workspaceApplyCmd.Flags().BoolVar(&workspaceApplyDryRun, "dry-run", false, "preview changes only, no writes")
	workspaceApplyCmd.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	workspaceCmd.AddCommand(workspaceApplyCmd)
}

func runWorkspaceApply(cmd *cobra.Command, args []string) error {
	if workspaceApplyFile == "" {
		return fmt.Errorf("-f/--file is required")
	}
	data, err := os.ReadFile(workspaceApplyFile)
	if err != nil {
		return fmt.Errorf("read -f/--file: %w", err)
	}

	// Parse the whole file up front (fail fast: a malformed document N
	// leaves document 1..N-1 untouched, matching the pre-existing
	// behavior) and also get each document's warning-relevant metadata
	// (AdditionalBindingsDropped) before any HTTP call is made.
	docs, err := orchestrator.DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", workspaceApplyFile, err)
	}

	// Split into each document's RAW bytes (not a re-marshal of the parsed
	// *WorkspaceEnvelopeApply values above) so POST /api/workspaces/apply
	// sees exactly the field presence the source file had — see
	// SplitWorkspaceEnvelopeDocuments's doc comment for why re-marshaling
	// the decoded struct would lose that distinction now that
	// WorkspaceEnvelopeSpec's fields no longer carry `omitempty` (Blocker 1).
	rawDocs, err := orchestrator.SplitWorkspaceEnvelopeDocuments(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", workspaceApplyFile, err)
	}
	if len(rawDocs) != len(docs) {
		return fmt.Errorf("internal error: parsed %d document(s) but split %d — %s may use a YAML feature these two decoders disagree on", len(docs), len(rawDocs), workspaceApplyFile)
	}

	c := client.FromContext(cmd.Context())
	out := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	for i, doc := range docs {
		slug := doc.Envelope.Metadata.Name
		if doc.AdditionalBindingsDropped {
			slog.Warn("workspace apply: spec.additional_bindings is no longer supported and was dropped", "workspace", slug)
			fmt.Fprintf(stderr, "warning: workspace %q: spec.additional_bindings is no longer supported (retired) — dropped\n", slug)
		}
		if err := applyOneWorkspaceDocument(c, out, stderr, slug, rawDocs[i], workspaceApplyDryRun); err != nil {
			return fmt.Errorf("apply workspace %q: %w", slug, err)
		}
	}
	return nil
}

// applyOneWorkspaceDocument POSTs raw (one document's original bytes) to
// POST /api/workspaces/apply?dry_run=<dryRun> — the daemon parses it,
// upserts the workspace metadata, and (if spec.projects is present)
// reconciles project assignments, all inside one transaction (docs/plans/
// volume-only-daemon.md PR-1d codex round-1 Blocker 2). Prints the same
// dry-run diff / created-or-updated / project attach-detach / missing-
// project warning output the pre-atomic client-side implementation did.
func applyOneWorkspaceDocument(c *client.Client, out, stderr io.Writer, slug string, raw []byte, dryRun bool) error {
	path := "/api/workspaces/apply"
	if dryRun {
		path += "?dry_run=true"
	}
	statusCode, respBody, err := c.PostRaw(path, "application/yaml", raw)
	if err != nil {
		return fmt.Errorf("apply request: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("apply: %s", formatWorkspaceAPIError(statusCode, respBody))
	}

	var result orchestrator.WorkspaceApplyResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode apply response: %w", err)
	}

	if dryRun {
		printWorkspaceApplyDiff(out, slug, !result.Created, result.Previous, result.Meta)
	} else if result.Created {
		fmt.Fprintf(out, "workspace %q created\n", slug)
	} else {
		fmt.Fprintf(out, "workspace %q updated\n", slug)
	}

	printWorkspaceApplyProjectChanges(out, dryRun, slug, &result)
	printWorkspaceApplyInitScriptChange(out, dryRun, slug, &result)
	// result.Warnings is deliberately NOT printed here: its doc comment
	// (orchestrator.WorkspaceApplyResult) scopes it to callers that hit the
	// endpoint directly, precisely because this command already emits the
	// same notices client-side — printing both would double every one of
	// them.
	for _, name := range result.MissingProjects {
		fmt.Fprintf(stderr, "warning: project '%s' referenced in workspace '%s' does not exist yet. Register it separately (or via PR-2's boid project add <url>) then re-apply.\n", name, slug)
	}
	return nil
}

// printWorkspaceApplyProjectChanges prints the project-assignment side
// effects (or, for dryRun, would-be side effects) of an apply — a project
// newly attached, one already correctly attached (dry-run preview only,
// matching the pre-atomic implementation's verbosity), and one detached
// (moved to the default workspace) because spec.projects was present but
// no longer listed it.
func printWorkspaceApplyProjectChanges(out io.Writer, dryRun bool, slug string, result *orchestrator.WorkspaceApplyResult) {
	verb := "attach"
	if dryRun {
		verb = "would attach"
	}
	for _, id := range result.AttachedProjects {
		fmt.Fprintf(out, "  project %q %sed to %q\n", id, verb, slug)
	}
	if dryRun {
		for _, id := range result.AlreadyAttachedProjects {
			fmt.Fprintf(out, "  project %q already attached to %q (no change)\n", id, slug)
		}
	}
	detachVerb := "detached"
	if dryRun {
		detachVerb = "would be detached"
	}
	for _, id := range result.DetachedProjects {
		fmt.Fprintf(out, "  project %q %s from %q (moved to %q)\n", id, detachVerb, slug, orchestrator.DefaultWorkspaceSlug)
	}
}

// printWorkspaceApplyInitScriptChange reports what the apply did (or would do)
// to the workspace's init.sh — PR9 of
// docs/plans/workspace-home-volume-persistence.md, 論点 d.
//
// Printed as its own line rather than folded into printWorkspaceApplyDiff's
// field list, because it is not a WorkspaceMeta field and did not land in the
// same transaction: it is a separate write to a file on the daemon, and a
// reader has to be able to see that. An empty InitScriptAction means the
// document carried no spec.init_script key at all — nothing to say.
//
// "unchanged" is printed too, unlike the project listing's already-attached
// case (dry-run only). Re-applying a backup and being told nothing about the
// script leaves "the script is identical" and "this daemon ignored the field"
// looking the same, and only one of them is fine.
func printWorkspaceApplyInitScriptChange(out io.Writer, dryRun bool, slug string, result *orchestrator.WorkspaceApplyResult) {
	if result.InitScriptAction == "" {
		return
	}
	verb := map[string]string{
		api.WorkspaceInitScriptWritten:   "written",
		api.WorkspaceInitScriptCleared:   "cleared (the workspace now runs no init script)",
		api.WorkspaceInitScriptUnchanged: "unchanged",
	}[result.InitScriptAction]
	if verb == "" {
		verb = result.InitScriptAction
	}
	if dryRun {
		fmt.Fprintf(out, "  init.sh would be %s\n", verb)
		return
	}
	fmt.Fprintf(out, "  init.sh %s\n", verb)
}

// printWorkspaceApplyDiff renders --dry-run's preview: per WorkspaceMeta
// field, a "-before"/"+after" pair when the field would change (silent when
// it would not). This is a structured, field-level diff rather than a full
// line-based (Myers/LCS) unified text diff — the source values are
// structured YAML data (lists/maps/scalars), not free text, so a field-level
// before/after is both simpler to implement correctly and, arguably, more
// legible than diffing two marshaled yaml blobs would be. Colored (red '-'/
// green '+') when out is a TTY, plain otherwise (docs/plans/
// volume-only-daemon.md PR-1d unilateral decision — flagged in the PR body).
func printWorkspaceApplyDiff(out io.Writer, slug string, exists bool, current, proposed *orchestrator.WorkspaceMeta) {
	color := diffColorEnabled(out)
	if exists {
		fmt.Fprintf(out, "~ workspace %q (update)\n", slug)
	} else {
		fmt.Fprintf(out, "+ workspace %q (create)\n", slug)
	}
	if current == nil {
		current = &orchestrator.WorkspaceMeta{}
	}
	if proposed == nil {
		proposed = &orchestrator.WorkspaceMeta{}
	}

	diffField(out, color, "host_commands", formatStringSlice(current.HostCommands), formatStringSlice(proposed.HostCommands))
	diffField(out, color, "env", formatEnvMapForDiff(current.Env), formatEnvMapForDiff(proposed.Env))
	diffField(out, color, "allowed_domains", formatStringSlice(current.AllowedDomains), formatStringSlice(proposed.AllowedDomains))
	diffField(out, color, "extra_repos", formatStringSlice(current.ExtraRepos), formatStringSlice(proposed.ExtraRepos))
	diffField(out, color, "services", formatStringSlice(current.Services), formatStringSlice(proposed.Services))
	diffField(out, color, "container_image", current.ContainerImage, proposed.ContainerImage)
	if !reflect.DeepEqual(current.Capabilities, proposed.Capabilities) {
		diffField(out, color, "capabilities.docker", fmt.Sprintf("%v", current.Capabilities.Docker != nil), fmt.Sprintf("%v", proposed.Capabilities.Docker != nil))
	}
}

// diffField prints one field's before/after pair, skipping fields that are
// unchanged.
func diffField(out io.Writer, color bool, name, before, after string) {
	if before == after {
		return
	}
	fmt.Fprintf(out, "  %s:\n", name)
	fmt.Fprintln(out, colorizeDiffLine(color, "31", "  - "+before))
	fmt.Fprintln(out, colorizeDiffLine(color, "32", "  + "+after))
}

// colorizeDiffLine wraps s in the given ANSI SGR code when enabled is true.
func colorizeDiffLine(enabled bool, code, s string) string {
	if !enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// diffColorEnabled reports whether out is a terminal (docs/plans/
// volume-only-daemon.md PR-1d unilateral decision: "colored unified diff,
// printed to stdout. If terminal not a TTY, plain text"). Only *os.File
// values can be a terminal; anything else (a *bytes.Buffer in tests, a pipe)
// is never colored.
func diffColorEnabled(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// formatEnvMapForDiff renders an env map deterministically (sorted
// "KEY=value" pairs, comma-joined) for diffField's before/after comparison
// and display — mirrors formatStringSlice's "(none)" convention for empty.
func formatEnvMapForDiff(env map[string]string) string {
	if len(env) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+env[k])
	}
	return formatStringSlice(pairs)
}
