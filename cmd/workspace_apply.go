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
	"gopkg.in/yaml.v3"
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
e.g. "env: {}" — clears it). Each spec.projects[] entry is attached to the
workspace by name if a project with that name is already registered;
otherwise a warning is printed and apply continues (see the plan doc's
§論点g for why registration-by-URL is not yet wired — that lands in PR-2).

--dry-run prints what would change without writing anything.`,
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

	docs, err := orchestrator.DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", workspaceApplyFile, err)
	}

	c := client.FromContext(cmd.Context())
	out := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	for _, doc := range docs {
		slug := doc.Envelope.Metadata.Name
		if doc.AdditionalBindingsDropped {
			slog.Warn("workspace apply: spec.additional_bindings is no longer supported and was dropped", "workspace", slug)
			fmt.Fprintf(stderr, "warning: workspace %q: spec.additional_bindings is no longer supported (retired) — dropped\n", slug)
		}
		if err := applyOneWorkspace(c, out, stderr, doc, workspaceApplyDryRun); err != nil {
			return fmt.Errorf("apply workspace %q: %w", slug, err)
		}
	}
	return nil
}

// applyOneWorkspace upserts a single workspace document: fetch-if-exists,
// merge (WorkspaceEnvelopeApply.MergeInto), then either print a dry-run diff
// or write it (PUT with If-Match for an existing workspace, POST for a new
// one — the same primitives `workspace edit`/`workspace create` already use;
// no atomic bulk endpoint was added, per the plan doc's RPC note allowing
// "single-workspace atomicity" when a bulk endpoint isn't otherwise needed).
// Project attachment (applyWorkspaceProjects) always runs after, dry-run or
// not, since it is a separate concern from the meta merge above.
func applyOneWorkspace(c *client.Client, out, stderr io.Writer, doc *orchestrator.WorkspaceEnvelopeApply, dryRun bool) error {
	slug := doc.Envelope.Metadata.Name

	statusCode, body, err := c.GetRaw("/api/workspaces/" + slug)
	if err != nil {
		return fmt.Errorf("check existing workspace: %w", err)
	}

	var exists bool
	var current api.WorkspaceDetail
	switch statusCode {
	case http.StatusOK:
		exists = true
		if err := json.Unmarshal(body, &current); err != nil {
			return fmt.Errorf("decode current workspace: %w", err)
		}
	case http.StatusNotFound:
		exists = false
	default:
		return fmt.Errorf("check existing workspace: %s", formatWorkspaceAPIError(statusCode, body))
	}

	var currentMeta *orchestrator.WorkspaceMeta
	if exists {
		currentMeta = current.Meta
	}
	merged := doc.MergeInto(currentMeta)

	if dryRun {
		printWorkspaceApplyDiff(out, slug, exists, currentMeta, merged)
	} else {
		metaYAML, err := yaml.Marshal(merged)
		if err != nil {
			return fmt.Errorf("marshal merged meta: %w", err)
		}
		if exists {
			statusCode, respBody, err := c.PutRawWithIfMatch("/api/workspaces/"+slug, "application/yaml", metaYAML, current.Revision)
			if err != nil {
				return fmt.Errorf("update workspace: %w", err)
			}
			if statusCode != http.StatusOK {
				return fmt.Errorf("update workspace: %s", formatWorkspaceAPIError(statusCode, respBody))
			}
			fmt.Fprintf(out, "workspace %q updated\n", slug)
		} else {
			createBody, err := buildWorkspaceCreateBody(slug, metaYAML)
			if err != nil {
				return fmt.Errorf("build create request: %w", err)
			}
			if err := c.DoWithContentType("POST", "/api/workspaces", "application/yaml", createBody, &api.WorkspaceDetail{}); err != nil {
				return fmt.Errorf("create workspace: %w", err)
			}
			fmt.Fprintf(out, "workspace %q created\n", slug)
		}
	}

	applyWorkspaceProjects(c, out, stderr, slug, doc.Envelope.Spec.Projects, dryRun)
	return nil
}

// applyWorkspaceProjects attaches each spec.projects[] entry to slug by
// name, via the same GET /api/projects/{ref} lookup `boid workspace assign`
// uses (resolveProjectRef's underlying endpoint) and PUT
// /api/projects/{id}/workspace. A project that does not resolve (404 unknown
// name, or 409 ambiguous name — both folded into the same "does not exist
// yet" message, since apply must never fail over this) prints a warning and
// continues, per docs/plans/volume-only-daemon.md §論点g: registering a
// project from spec.projects[].url is PR-2 scope, not this one.
func applyWorkspaceProjects(c *client.Client, out, stderr io.Writer, slug string, projects []orchestrator.WorkspaceEnvelopeProject, dryRun bool) {
	for _, ep := range projects {
		statusCode, body, err := c.GetRaw("/api/projects/" + ep.Name)
		if err != nil {
			fmt.Fprintf(stderr, "warning: workspace %q: resolve project %q failed: %v\n", slug, ep.Name, err)
			continue
		}

		switch statusCode {
		case http.StatusOK:
			var p orchestrator.Project
			if err := json.Unmarshal(body, &p); err != nil {
				fmt.Fprintf(stderr, "warning: workspace %q: decode project %q failed: %v\n", slug, ep.Name, err)
				continue
			}
			if p.WorkspaceID == slug {
				if dryRun {
					fmt.Fprintf(out, "  project %q already attached to %q (no change)\n", ep.Name, slug)
				}
				continue
			}
			if dryRun {
				fmt.Fprintf(out, "  ~ attach project %q (currently in %q) to %q\n", ep.Name, p.WorkspaceID, slug)
				continue
			}
			var updated orchestrator.Project
			if err := c.Do("PUT", "/api/projects/"+p.ID+"/workspace", map[string]string{"workspace_id": slug}, &updated); err != nil {
				fmt.Fprintf(stderr, "warning: workspace %q: attach project %q failed: %v\n", slug, ep.Name, err)
				continue
			}
			fmt.Fprintf(out, "  project %q attached to %q\n", ep.Name, slug)

		case http.StatusNotFound, http.StatusConflict:
			fmt.Fprintf(stderr, "warning: project '%s' referenced in workspace '%s' does not exist yet. Register it separately (or via PR-2's boid project add <url>) then re-apply.\n", ep.Name, slug)

		default:
			fmt.Fprintf(stderr, "warning: workspace %q: resolve project %q: %s\n", slug, ep.Name, formatWorkspaceAPIError(statusCode, body))
		}
	}
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

	diffField(out, color, "host_commands", formatStringSlice(current.HostCommands), formatStringSlice(proposed.HostCommands))
	diffField(out, color, "env", formatEnvMapForDiff(current.Env), formatEnvMapForDiff(proposed.Env))
	diffField(out, color, "allowed_domains", formatStringSlice(current.AllowedDomains), formatStringSlice(proposed.AllowedDomains))
	diffField(out, color, "extra_repos", formatStringSlice(current.ExtraRepos), formatStringSlice(proposed.ExtraRepos))
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
