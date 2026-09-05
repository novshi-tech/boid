package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/humanize"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Manage local workspace groupings",
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workspaces (GET /api/workspaces)",
	RunE:  runWorkspaceList,
}

var workspaceShowCmd = &cobra.Command{
	Use:   "show <slug>",
	Short: "Show a workspace's definition and assigned projects (GET /api/workspaces/{slug})",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceShow,
}

// workspaceCreateFromFile is the --from-file flag value for `workspace
// create`: an optional yaml document describing the new workspace's meta
// fields. Empty creates a blank workspace.
var workspaceCreateFromFile string

var workspaceCreateCmd = &cobra.Command{
	Use:   "create <slug>",
	Short: "Create a new workspace (POST /api/workspaces; empty, or from --from-file yaml)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceCreate,
}

var (
	// workspaceEditFromFile is the required --from-file flag value for
	// `workspace edit`: the yaml document that replaces the workspace's
	// meta wholesale (no individual field flags).
	workspaceEditFromFile string
	// workspaceEditForce skips the automatic If-Match revision check, for a
	// deliberate last-write-wins edit.
	workspaceEditForce bool
)

var workspaceEditCmd = &cobra.Command{
	Use:   "edit <slug> --from-file <yaml>",
	Short: "Replace a workspace's definition wholesale (PUT /api/workspaces/{slug}, automatic If-Match)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceEdit,
}

var workspaceAssignCmd = &cobra.Command{
	Use:   "assign <project-ref> <slug>",
	Short: "Assign a project to a workspace (auto-creates the workspace from a local workspace.yaml if one exists but has no DB row yet)",
	Args:  cobra.ExactArgs(2),
	RunE:  runWorkspaceAssign,
}

var workspaceClearCmd = &cobra.Command{
	Use:   "clear <project-ref>",
	Short: "Reset a project's workspace assignment to the default workspace",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceClear,
}

var workspaceRemoveCmd = &cobra.Command{
	Use:     "remove <slug>",
	Aliases: []string{"delete"},
	Short:   "Remove a workspace and its HOME volume (DELETE /api/workspaces/{slug}; drops assigned projects back to \"default\"; always prompts for confirmation; --force/--yes skips the prompt)",
	Args:    cobra.ExactArgs(1),
	RunE:    runWorkspaceRemove,
}

// workspaceExportOutput is the optional -o/--output flag value for
// `workspace export`: a file path to write the exported yaml to, instead of
// stdout.
var workspaceExportOutput string

// workspaceExportAll is the --all flag value for `workspace export`: export
// every workspace instead of the single <slug> named as a positional
// argument.
var workspaceExportAll bool

var workspaceExportCmd = &cobra.Command{
	Use:   "export <slug> [--all] [-o file.yaml]",
	Short: "Export workspace(s) + their assigned projects as a boid.dev/v1 Workspace yaml document",
	Long: `Export a workspace's definition — host_commands, env, allowed_domains,
capabilities, and its assigned projects (name + git URL when known) — as a
self-describing "apiVersion: boid.dev/v1 / kind: Workspace" yaml document
(docs/plans/volume-only-daemon.md §論点g).

"boid workspace export <slug>" exports one workspace. "boid workspace export
--all" exports every workspace into a single "---"-separated multi-document
file, suitable for "boid workspace apply -f" on a fresh install.

"boid workspace export --all" is the ONLY endorsed backup path for workspace
+ project-assignment state — a raw copy of the daemon's database/volume is
NOT a valid restore mechanism on its own (task/job history and other DB
state are treated as volatile; see the plan doc's "backup 契約" decision).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWorkspaceExport,
}

// workspaceImportMode/workspaceImportForce/workspaceImportSlug are
// `workspace import`'s pre-deprecation flags (--mode/--force/--slug). The
// command's RunE (runWorkspaceImportDeprecated) no longer reads them — the
// meta-format import it used to drive was retired (see that function's doc
// comment). They stay registered only so an existing `boid workspace import
// <file> --mode ... --slug ...` invocation still PARSES successfully and
// reaches the deprecation guidance below, instead of cobra rejecting an
// unrecognized flag before RunE gets a chance to say where the
// functionality moved.
var (
	workspaceImportMode  string
	workspaceImportForce bool
	workspaceImportSlug  string
)

// workspaceImportCmd is retired: the meta-format round trip it and
// GET /api/workspaces/{slug}/export used to share never actually worked end
// to end (see runWorkspaceImportDeprecated's doc comment for the concrete
// failures). Kept as a Hidden command rather than removed outright, so a
// caller relying on old muscle memory or a stale script is told where the
// functionality moved instead of hitting cobra's generic "unknown command".
//
// workspaceImportCmd's Annotations are set separately from the shared
// scopeRemote loop in init() below: unlike every other workspace
// subcommand, it no longer talks to a daemon at all — its RunE
// (runWorkspaceImportDeprecated) always fails before making any HTTP call.
// Left in the scopeRemote loop, PersistentPreRunE (cmd/root.go) would still
// autostart a daemon (or reject a non-unix profile as if this were a live
// remote operation) BEFORE RunE ever gets a chance to print the deprecation
// guidance. scopeLocal + annotationSkipAutostart=skip, mirroring the other
// deprecated stub in this tree (`boid init`, cmd/init.go), makes this
// exactly as inert as any other command that never reaches the network.
var workspaceImportCmd = &cobra.Command{
	Use:    "import <file>",
	Short:  "(廃止) boid workspace apply -f / create,edit --from-file を使ってください",
	Hidden: true,
	Args:   cobra.ArbitraryArgs,
	RunE:   runWorkspaceImportDeprecated,
	// The usage block cobra prints after a RunE error would otherwise follow
	// the deprecation message with a full flag listing that reads like this
	// command still does something — silence it, matching the message's own
	// "this command always fails" posture.
	SilenceUsage: true,
}

func init() {
	// Every workspace subcommand talks to the daemon's HTTP API — all
	// scopeRemote — EXCEPT workspaceImportCmd, annotated separately below
	// (see its own doc comment for why).
	for _, c := range []*cobra.Command{
		workspaceListCmd, workspaceShowCmd, workspaceCreateCmd, workspaceEditCmd,
		workspaceAssignCmd, workspaceClearCmd, workspaceRemoveCmd,
		workspaceExportCmd,
	} {
		c.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	}
	workspaceImportCmd.Annotations = map[string]string{
		annotationSkipAutostart: "skip",
		scopeAnnotationKey:      scopeLocal,
	}

	workspaceCreateCmd.Flags().StringVar(&workspaceCreateFromFile, "from-file", "", "yaml file describing the workspace meta (optional; omit to create a blank workspace)")
	workspaceEditCmd.Flags().StringVar(&workspaceEditFromFile, "from-file", "", "yaml file with the new workspace meta (required)")
	workspaceEditCmd.Flags().BoolVar(&workspaceEditForce, "force", false, "skip the If-Match revision check (last-write-wins)")
	workspaceExportCmd.Flags().StringVarP(&workspaceExportOutput, "output", "o", "", "file path to write the exported yaml to (default: stdout)")
	workspaceExportCmd.Flags().BoolVar(&workspaceExportAll, "all", false, "export every workspace into a single '---'-separated multi-document file, instead of the <slug> positional argument")
	workspaceImportCmd.Flags().StringVar(&workspaceImportMode, "mode", "create-only", "(no longer used; accepted only so an existing invocation still parses and reaches the deprecation guidance)")
	workspaceImportCmd.Flags().BoolVar(&workspaceImportForce, "force", false, "(no longer used; accepted only so an existing invocation still parses and reaches the deprecation guidance)")
	workspaceImportCmd.Flags().StringVar(&workspaceImportSlug, "slug", "", "(no longer used; accepted only so an existing invocation still parses and reaches the deprecation guidance)")
	workspaceRemoveCmd.Flags().BoolVar(&workspaceRemoveForce, "force", false, "skip the home volume deletion confirmation prompt")
	workspaceRemoveCmd.Flags().BoolVar(&workspaceRemoveForce, "yes", false, "alias for --force")

	workspaceCmd.AddCommand(
		workspaceListCmd,
		workspaceShowCmd,
		workspaceCreateCmd,
		workspaceEditCmd,
		workspaceAssignCmd,
		workspaceClearCmd,
		workspaceRemoveCmd,
		workspaceExportCmd,
		workspaceImportCmd,
	)
	rootCmd.AddCommand(workspaceCmd)
}

// runWorkspaceList lists every workspace via GET /api/workspaces: the
// workspaces table is the single source of truth, so this needs no local
// filesystem read at all — an empty workspace (no assigned projects) is
// already included in the API response.
func runWorkspaceList(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	var workspaces []*orchestrator.WorkspaceSummary
	if err := c.Do("GET", "/api/workspaces", nil, &workspaces); err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	if workspaces == nil {
		workspaces = []*orchestrator.WorkspaceSummary{}
	}

	return renderOutput(cmd, workspaces, func() error {
		out := cmd.OutOrStdout()
		if len(workspaces) == 0 {
			fmt.Fprintln(out, "no workspaces configured")
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SLUG\tPROJECTS\tREVISION")
		for _, ws := range workspaces {
			fmt.Fprintf(tw, "%s\t%d\t%s\n", ws.ID, ws.ProjectCount, ws.Revision)
		}
		return tw.Flush()
	})
}

// workspaceShowView is the JSON shape for workspace show output.
type workspaceShowView struct {
	Slug     string                      `json:"slug"`
	Meta     *orchestrator.WorkspaceMeta `json:"meta,omitempty"`
	Revision string                      `json:"revision,omitempty"`
	// Home reports the workspace HOME VOLUME's size as the engine sees it,
	// mirrored straight from the GET /api/workspaces/{slug} response.
	Home     *apiwire.WorkspaceHomeSize `json:"home,omitempty"`
	Projects []*orchestrator.Project    `json:"projects"`
}

// runWorkspaceShow shows a workspace's definition (GET /api/workspaces/{slug})
// alongside its assigned projects (GET /api/projects?workspace_id=<slug> —
// kept so the project listing can still show each project's WorkDir, which
// the workspace endpoint's AssignedProjects (project ids only) does not
// carry). A workspace that only exists as a local workspace.yaml (never
// assigned or `boid workspace create`d) 404s here, matching the
// DB-is-authority contract.
func runWorkspaceShow(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.FromContext(cmd.Context())

	var detail apiwire.WorkspaceDetail
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &detail); err != nil {
		return fmt.Errorf("show workspace: %w", err)
	}

	var projects []*orchestrator.Project
	if err := c.Do("GET", "/api/projects?workspace_id="+slug, nil, &projects); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	if projects == nil {
		projects = []*orchestrator.Project{}
	}

	view := workspaceShowView{
		Slug:     slug,
		Meta:     detail.Meta,
		Revision: detail.Revision,
		Home:     detail.Home,
		Projects: projects,
	}

	return renderOutput(cmd, view, func() error {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Workspace: %s\n", slug)
		if view.Revision != "" {
			fmt.Fprintf(out, "revision: %s\n", view.Revision)
		}

		if meta := view.Meta; meta != nil {
			fmt.Fprintf(out, "host_commands: %s\n", formatStringSlice(meta.HostCommands))
			fmt.Fprintf(out, "services: %s\n", formatStringSlice(meta.Services))
			if len(meta.Env) > 0 {
				envKeys := make([]string, 0, len(meta.Env))
				for k := range meta.Env {
					envKeys = append(envKeys, k)
				}
				sort.Strings(envKeys)
				fmt.Fprintln(out, "env:")
				for _, k := range envKeys {
					fmt.Fprintf(out, "  %s: %s\n", k, meta.Env[k])
				}
			}
			if meta.Capabilities.Docker != nil {
				fmt.Fprintf(out, "capabilities: docker=enabled\n")
			}
			fmt.Fprintln(out)
		}

		if view.Home != nil {
			fmt.Fprintln(out, formatWorkspaceHomeSize(view.Home))
		}

		if len(projects) == 0 {
			fmt.Fprintf(out, "projects: (none) — `boid workspace remove %s` で削除可能\n", slug)
		} else {
			fmt.Fprintf(out, "projects (%d):\n", len(projects))
			for _, p := range projects {
				fmt.Fprintf(out, "  %-36s  %s\n", p.ID, filepath.Base(p.WorkDir))
			}
		}
		return nil
	})
}

// runWorkspaceCreate creates a new workspace via POST /api/workspaces.
// --from-file is optional: omitted, it creates a blank workspace; given,
// its yaml content is merged with the target slug into the single combined
// body the create endpoint expects.
func runWorkspaceCreate(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	var metaYAML []byte
	if workspaceCreateFromFile != "" {
		data, err := os.ReadFile(workspaceCreateFromFile)
		if err != nil {
			return fmt.Errorf("read --from-file: %w", err)
		}
		metaYAML = data

		// Validate --from-file with the same strict (multi-document-rejecting)
		// decoder the server uses, before buildWorkspaceCreateBody's loose
		// map[string]any merge below gets a chance to silently drop a second
		// "---"-delimited document. Client-side fail-fast only — the server
		// performs the same validation again on the constructed body.
		if _, err := orchestrator.DecodeWorkspaceMetaStrict(metaYAML); err != nil {
			return fmt.Errorf("validate --from-file: %w", err)
		}
	}

	body, err := buildWorkspaceCreateBody(slug, metaYAML)
	if err != nil {
		return fmt.Errorf("build create request: %w", err)
	}

	c := client.FromContext(cmd.Context())
	var detail apiwire.WorkspaceDetail
	if err := c.DoWithContentType("POST", "/api/workspaces", "application/yaml", body, &detail); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}

	return renderOutput(cmd, &detail, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace created: %s (revision %s)\n", detail.Slug, detail.Revision)
		return nil
	})
}

// buildWorkspaceCreateBody merges slug into metaYAML's top-level mapping,
// producing the single yaml document POST /api/workspaces expects
// (top-level "slug:" key alongside the meta fields — see
// orchestrator.DecodeWorkspaceCreateStrict). metaYAML may be empty (a blank
// workspace) or the raw content of a --from-file document, whose top level
// must itself be a mapping (the same shape `boid workspace edit` accepts).
//
// The strict decoder is invoked on metaYAML first: the server's
// DecodeWorkspaceCreateStrict runs against the fully-marshalled body below,
// but yaml.Unmarshal only reads the *first* document — so a caller passing
// a two-document file would silently drop the second one before the
// server ever sees it. Validating metaYAML up-front (using a strict
// decoder that rejects trailing documents and unknown nested fields)
// closes that hole.
func buildWorkspaceCreateBody(slug string, metaYAML []byte) ([]byte, error) {
	if len(bytes.TrimSpace(metaYAML)) > 0 {
		if _, err := orchestrator.DecodeWorkspaceMetaStrict(metaYAML); err != nil {
			return nil, fmt.Errorf("parse --from-file: %w", err)
		}
	}
	fields := map[string]any{}
	if len(bytes.TrimSpace(metaYAML)) > 0 {
		if err := yaml.Unmarshal(metaYAML, &fields); err != nil {
			return nil, fmt.Errorf("parse --from-file: %w", err)
		}
	}
	fields["slug"] = slug
	return yaml.Marshal(fields)
}

// runWorkspaceEdit replaces a workspace's definition wholesale via
// PUT /api/workspaces/{slug} (--from-file only, no individual field flags).
// Unless --force is set, the current revision is fetched first (a plain
// GET) and sent back as If-Match — the CLI attaches the ETag automatically
// so the common case ("edit what I just saw") never needs the caller to
// juggle revisions by hand; --force skips this for a deliberate
// last-write-wins edit.
func runWorkspaceEdit(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if workspaceEditFromFile == "" {
		return fmt.Errorf("--from-file is required")
	}
	data, err := os.ReadFile(workspaceEditFromFile)
	if err != nil {
		return fmt.Errorf("read --from-file: %w", err)
	}

	// Fail fast on a multi-document --from-file before making any daemon
	// call. The server (PUT /api/workspaces/{slug}) already runs this exact
	// same check on the raw body this function forwards verbatim, so this is
	// a client-side convenience — it saves a round trip (including the
	// automatic revision GET below) and reports the failure without needing
	// the daemon reachable at all.
	if _, err := orchestrator.DecodeWorkspaceMetaStrict(data); err != nil {
		return fmt.Errorf("validate --from-file: %w", err)
	}

	c := client.FromContext(cmd.Context())

	var ifMatch string
	if !workspaceEditForce {
		var current apiwire.WorkspaceDetail
		if err := c.Do("GET", "/api/workspaces/"+slug, nil, &current); err != nil {
			return fmt.Errorf("fetch current revision: %w", err)
		}
		ifMatch = current.Revision
	}

	path := "/api/workspaces/" + slug
	if workspaceEditForce {
		path += "?force=true"
	}
	statusCode, body, err := c.PutRawWithIfMatch(path, "application/yaml", data, ifMatch)
	if err != nil {
		return fmt.Errorf("edit workspace: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("edit workspace: %s", formatWorkspaceAPIError(statusCode, body))
	}

	var detail apiwire.WorkspaceDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	return renderOutput(cmd, &detail, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace updated: %s (revision %s)\n", detail.Slug, detail.Revision)
		return nil
	})
}

// formatWorkspaceAPIError renders a raw (statusCode, body) pair from
// PutRawWithIfMatch into a human-readable message, extracting the
// `{"error": "..."}` shape writeError produces when present.
func formatWorkspaceAPIError(statusCode int, body []byte) string {
	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
		return fmt.Sprintf("HTTP %d: %s", statusCode, errResp.Error)
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

// runWorkspaceAssign assigns a project to a workspace
// (PUT /api/projects/{id}/workspace). The daemon enforces an existence
// check on this endpoint, so assigning to an unknown slug 404s — unless a
// local workspace.yaml for that slug already exists (e.g. written by hand,
// by an e2e scenario, or by the now-retired `boid workspace configure`
// command), in which case ensureWorkspaceExistsForAssign auto-creates the
// DB row from it first so the existing "drop a yaml, then assign" flow
// keeps working end to end.
func runWorkspaceAssign(cmd *cobra.Command, args []string) error {
	// CLI entry-point validation per plan (3-layer defense). Early error gives
	// a better UX than a 400 from the daemon.
	slug := args[1]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	if err := ensureWorkspaceExistsForAssign(c, slug, cmd.OutOrStdout()); err != nil {
		return err
	}

	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+p.ID+"/workspace", map[string]string{"workspace_id": slug}, &project); err != nil {
		return fmt.Errorf("assign workspace: %w", err)
	}

	return renderOutput(cmd, &project, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace assigned: %s -> %s\n", project.ID, project.WorkspaceID)
		return nil
	})
}

// resolveDaemonKitsDir returns the daemon's effective KitsDir via
// GET /api/config/kits-dir.
//
// Every failure mode — a 404 (the daemon does not expose this endpoint at
// all), a 5xx, a transport failure, a response body that does not decode
// as the expected shape, or a body that decodes cleanly but reports an
// empty kits_dir — returns a hard error instead of silently falling back
// to this CLI process's own defaultKitsDir() computation: silently
// substituting this CLI's own default risks resolving (and then
// permanently persisting, via MaterializeWorkspaceKitsForPersist) a
// workspace's kit references against the wrong directory whenever a
// same-named kit happens to exist under both locations. A daemon started
// with `boid start --kits-dir <custom>` still resolves correctly (the
// common, successful 200 case); only when this CLI genuinely cannot learn
// the real answer does it now refuse to guess.
func resolveDaemonKitsDir(c *client.Client) (string, error) {
	statusCode, body, err := c.GetRaw("/api/config/kits-dir")
	if err != nil {
		return "", fmt.Errorf("fetch daemon kits-dir: %w", err)
	}
	if statusCode == http.StatusNotFound {
		return "", fmt.Errorf("fetch daemon kits-dir: daemon does not expose GET /api/config/kits-dir (HTTP 404) — upgrade the daemon (`boid start`) to a version that supports Phase 2.5 PR7 before assigning a workspace whose local yaml references a kit")
	}
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("fetch daemon kits-dir: HTTP %d: %s", statusCode, strings.TrimSpace(string(body)))
	}
	var resp struct {
		KitsDir string `json:"kits_dir"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("fetch daemon kits-dir: decode response: %w", err)
	}
	if resp.KitsDir == "" {
		return "", fmt.Errorf("fetch daemon kits-dir: daemon reported an empty kits_dir")
	}
	return resp.KitsDir, nil
}

// localWorkspaceYAMLReadFile reads a local workspace yaml file's raw bytes.
// Indirected through a package-level variable rather than calling
// os.ReadFile directly solely so tests can pin
// ensureWorkspaceExistsForAssign's TOCTOU-avoidance invariant — that it
// reads workspaceDir/slug.yaml exactly once — by counting calls. Mirrors
// workspace_migration.go's readWorkspaceYAMLSnapshot / workspaceYAMLReadFile
// var (server side) for the identical reason.
var localWorkspaceYAMLReadFile = os.ReadFile

// ensureWorkspaceExistsForAssign implements `boid workspace assign`'s
// auto-create pre-check: if slug already has a workspaces row, this is a
// no-op. Otherwise, if a legacy local workspace.yaml exists for slug
// (dropped by hand, by an e2e scenario, or by the now-retired `boid
// workspace configure` command — which only ever wrote the local yaml,
// never a DB row), its content is POSTed to the daemon so the assignment's
// existence check can succeed. If neither exists, this is a silent no-op:
// the subsequent assign call surfaces whatever the real outcome is (a
// plain 404 for a genuinely unknown slug).
//
// A local workspace.yaml read failure other than "file does not exist" (a
// parse error, or a permission error) is NOT silently swallowed as "no
// local yaml either" — that would make a real config or filesystem problem
// indistinguishable from the legitimate "nothing to auto-create from"
// case, surfacing only as a confusing 404 from the *subsequent* assign
// call instead of the actual cause. Only os.ErrNotExist falls through to
// that silent path; anything else is returned so the CLI reports the real
// error.
//
// workspaceDir/slug.yaml is read exactly ONCE: reading it twice (once to
// load loosely, once more to extract a legacy `kits:` list and strictly
// validate the remainder) risks an atomic rename landing between the two
// reads and handing this function a "meta from the old file version + kits
// from the new file version" hybrid that never existed on disk at any
// single instant. Reading the raw bytes once and deriving both kitRefs
// (extractLegacyWorkspaceKitRefs) and meta (DecodeWorkspaceMetaStrict,
// which conveniently already both validates AND decodes) from that single
// snapshot makes that impossible.
func ensureWorkspaceExistsForAssign(c *client.Client, slug string, out io.Writer) error {
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &apiwire.WorkspaceDetail{}); err == nil {
		return nil // already has a DB row.
	}

	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		return fmt.Errorf("resolve workspace dir for auto-create: %w", err)
	}

	raw, err := localWorkspaceYAMLReadFile(filepath.Join(wsDir, slug+".yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no local yaml either — let the assign call's own error surface.
		}
		return fmt.Errorf("read local workspace.yaml %q for auto-create: %w", slug, err)
	}

	// WorkspaceMeta no longer has a Kits field, and the server's strict
	// decoder no longer accepts a `kits:` key at all — extractLegacyWorkspaceKitRefs
	// pulls the kits: list (if any) out of THIS SAME raw snapshot, and
	// DecodeWorkspaceMetaStrict below decodes the kits-stripped remainder
	// from it too, so meta and kitRefs can never diverge from what was
	// actually on disk at the moment of the one read above.
	kitRefs, rest, err := extractLegacyWorkspaceKitRefs(raw)
	if err != nil {
		return fmt.Errorf("parse local workspace.yaml %q for auto-create: %w", slug, err)
	}

	// DecodeWorkspaceMetaStrict both validates (typo'd/unknown fields,
	// multi-document bodies) AND decodes rest into the WorkspaceMeta used
	// below.
	meta, err := orchestrator.DecodeWorkspaceMetaStrict(rest)
	if err != nil {
		return fmt.Errorf("validate local workspace.yaml %q for auto-create: %w", slug, err)
	}

	// An unresolvable kit ref aborts the auto-create instead of being
	// swallowed as a "note:" and silently creating the workspace without
	// the kit's host_commands/env/bindings: the workspace this function is
	// about to POST would silently omit content the on-disk `kits:`
	// reference explicitly asked for, and the DB row created from it can
	// never be re-materialized afterward (MaterializeWorkspaceKitsForPersist
	// is a client-side, create-time-only expansion; nothing re-runs it once
	// the row exists).
	if len(kitRefs) > 0 {
		kitsDir, err := resolveDaemonKitsDir(c)
		if err != nil {
			return fmt.Errorf("resolve workspace %q's kits for auto-create: %w", slug, err)
		}
		if err := orchestrator.MaterializeWorkspaceKitsForPersist(kitsDir, kitRefs, meta); err != nil {
			return fmt.Errorf("resolve workspace %q's kits for auto-create: %w", slug, err)
		}
	}

	data, err := yaml.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal local workspace.yaml for auto-create: %w", err)
	}
	postWorkspaceCreateBestEffort(c, slug, data, out, "from local workspace.yaml")
	return nil
}

// extractLegacyWorkspaceKitRefs pulls a legacy top-level `kits:` list out of
// raw local workspace yaml, returning the kit ref names alongside a copy of
// raw with the kits: key removed. WorkspaceMeta (and its strict wire
// counterpart, workspaceMetaStrict) no longer has a Kits field at all —
// orchestrator.DecodeWorkspaceMetaStrict now rejects a `kits:` key outright
// ("unknown field kits") — so ensureWorkspaceExistsForAssign, the one
// remaining caller that still needs to honor it (`boid workspace assign`'s
// auto-create convenience path against a hand-authored or e2e-fixture shadow
// yaml), extracts it here before running that strict validation against the
// remainder.
//
// An absent kits key returns (nil, raw, nil) unchanged — the fast path, and
// the common case for anything authored post-cutover.
//
// A second "---"-delimited document in raw is rejected up front, before any
// unmarshal-to-map-then-remarshal happens below: deciding trailing-document-
// ness from raw directly (rather than from a fresh marshal of an
// already-truncated map) is what lets the caller's later
// DecodeWorkspaceMetaStrict(rest) call still see a dropped second document.
func extractLegacyWorkspaceKitRefs(raw []byte) (kitRefs []string, rest []byte, err error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, raw, nil // empty document — no kits key, nothing to strip.
		}
		return nil, raw, err
	}
	if err := orchestrator.RejectTrailingYAMLDocument(dec); err != nil {
		return nil, raw, err
	}
	kitsVal, ok := doc["kits"]
	if !ok {
		return nil, raw, nil
	}
	// `kits:` (bare, value omitted) and `kits: null` both parse to nil under
	// yaml.v3 map decoding; treat them the same as the key being absent
	// here, since existing shadow yaml files or hand-typed configs still
	// legitimately carry this shape. Splitting the assertion also strips the
	// now-redundant `kits: null` line from the outgoing body via delete(doc).
	if kitsVal == nil {
		delete(doc, "kits")
		rest, err = yaml.Marshal(doc)
		if err != nil {
			return nil, raw, err
		}
		return nil, rest, nil
	}
	items, ok := kitsVal.([]any)
	if !ok {
		return nil, raw, fmt.Errorf("kits: expected a list of strings, got %T", kitsVal)
	}
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return nil, raw, fmt.Errorf("kits: expected a list of strings, got element of type %T", item)
		}
		kitRefs = append(kitRefs, name)
	}
	delete(doc, "kits")
	rest, err = yaml.Marshal(doc)
	if err != nil {
		return nil, raw, err
	}
	return kitRefs, rest, nil
}

// postWorkspaceCreateBestEffort POSTs slug (with metaYAML, which may be
// empty/nil for a blank workspace) to /api/workspaces, tolerating any
// failure as best-effort: this create is a convenience so a subsequent
// assign's existence check succeeds, not something worth hard-failing the
// CLI command over. A concurrent creator winning the create race (409), or
// any other daemon-side issue, is reported as an informational note on out
// rather than an error — the caller's own follow-up assign call is what
// actually needs slug to exist, and will surface a sharp error if it still
// does not. Shared by ensureWorkspaceExistsForAssign (`boid workspace
// assign`) and ensureWorkspaceExistsGetOrCreate (`boid project add
// --workspace`) — the two differ only in what metaYAML (if any) they
// create from and how they describe the source in the printed note.
func postWorkspaceCreateBestEffort(c *client.Client, slug string, metaYAML []byte, out io.Writer, sourceDescription string) {
	body, err := buildWorkspaceCreateBody(slug, metaYAML)
	if err != nil {
		fmt.Fprintf(out, "note: build auto-create request for workspace %q failed: %v\n", slug, err)
		return
	}
	if err := c.DoWithContentType("POST", "/api/workspaces", "application/yaml", body, &apiwire.WorkspaceDetail{}); err != nil {
		fmt.Fprintf(out, "note: auto-create workspace %q %s failed: %v\n", slug, sourceDescription, err)
		return
	}
	fmt.Fprintf(out, "workspace %q auto-created %s\n", slug, sourceDescription)
}

// ensureWorkspaceExistsGetOrCreate implements `boid project add --workspace`'s
// get-or-create contract: an empty workspace is created unconditionally
// when slug has no DB row yet — unlike `boid workspace assign`'s
// auto-create (ensureWorkspaceExistsForAssign), which only fires for a
// slug with a pre-existing legacy workspace.yaml and otherwise silently
// no-ops so a genuinely unknown slug still 404s on assign.
//
// `project init --workspace` does not call this: its --workspace flag only
// feeds the `boid project add <url> --workspace=<name>` command it prints
// as guidance, so the actual get-or-create happens later, inside a real
// `project add` invocation, through this same function.
func ensureWorkspaceExistsGetOrCreate(c *client.Client, slug string, out io.Writer) error {
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &apiwire.WorkspaceDetail{}); err == nil {
		return nil // already exists.
	}
	postWorkspaceCreateBestEffort(c, slug, nil, out, "(empty, get-or-create)")
	return nil
}

func runWorkspaceClear(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	// "Clear" now resets to the default workspace rather than removing the
	// project_workspaces row. Every project belongs to exactly one workspace
	// — "unassigned" is no longer a representable state.
	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+p.ID+"/workspace",
		map[string]string{"workspace_id": orchestrator.DefaultWorkspaceSlug}, &project); err != nil {
		return fmt.Errorf("clear workspace: %w", err)
	}

	return renderOutput(cmd, &project, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace reset to %s: %s\n", orchestrator.DefaultWorkspaceSlug, project.ID)
		return nil
	})
}

// workspaceRemoveForce is the --force (alias --yes) flag value for
// `workspace remove`: skips the home-directory deletion confirmation prompt.
var workspaceRemoveForce bool

// workspaceRemoveConfirmPrompt reads the y/N answer to `workspace remove`'s
// home-directory deletion confirmation. A package-level var (rather than a
// direct call) so tests can stub it without a real TTY.
var workspaceRemoveConfirmPrompt = defaultWorkspaceRemoveConfirmPrompt

// defaultWorkspaceRemoveConfirmPrompt reads a y/N answer from in. Anything
// other than "y"/"yes" (case-insensitive) is treated as decline.
func defaultWorkspaceRemoveConfirmPrompt(in io.Reader) (bool, error) {
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return false, nil
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes", nil
}

// runWorkspaceRemove deletes a workspace via DELETE /api/workspaces/{slug}.
// This does not block on (or even check for) assigned projects first:
// WorkspaceRepository.Remove's transaction already re-assigns any assigned
// project to the default workspace as part of the same delete, so there is
// nothing left to clear by hand first.
//
// Unless --force is given, the workspace's current HOME VOLUME size is
// fetched first (GET /api/workspaces/{slug}, the same `workspace show`
// endpoint — no separate dry-run endpoint needed) and a y/N confirmation is
// required before the DELETE call is made at all.
//
// The prompt is unconditional — it does not skip just because GET reported
// Exists=false: a home that does not exist *yet* at GET time can still
// exist by the time DELETE runs, if a concurrent dispatch job creates it in
// between, and skipping the prompt in that window would let DELETE
// silently destroy data GET never got a chance to show the operator. This
// is a minimal defense, not a full fix (there remains an open daemon-side
// lifecycle-lock gap this does not close). The home size line stays purely
// informational: "home 未作成" when nothing has been dispatched into the
// workspace yet, rather than gating whether the prompt itself appears.
func runWorkspaceRemove(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	// CLI entry-point guard (3-layer defense; the domain layer —
	// orchestrator.WorkspaceRepository.Remove — enforces the same rule as
	// the last line of defense). The reserved default workspace cannot be
	// removed because every project would otherwise become unlinked.
	if slug == orchestrator.DefaultWorkspaceSlug {
		return fmt.Errorf("workspace %q is reserved and cannot be removed", slug)
	}

	c := client.FromContext(cmd.Context())
	out := cmd.OutOrStdout()

	if !workspaceRemoveForce {
		var detail apiwire.WorkspaceDetail
		if err := c.Do("GET", "/api/workspaces/"+slug, nil, &detail); err != nil {
			return fmt.Errorf("fetch workspace before remove: %w", err)
		}
		if detail.Home != nil {
			fmt.Fprintln(out, formatWorkspaceHomeSize(detail.Home))
		}
		fmt.Fprintf(out, "workspace remove %q — 本当に削除しますか? [y/N]: ", slug)
		proceed, err := workspaceRemoveConfirmPrompt(os.Stdin)
		if err != nil {
			return fmt.Errorf("confirm prompt: %w", err)
		}
		if !proceed {
			fmt.Fprintf(out, "aborted: workspace %q was not removed\n", slug)
			return nil
		}
	}

	var resp apiwire.WorkspaceRemoveResponse
	if err := c.Do("DELETE", "/api/workspaces/"+slug, nil, &resp); err != nil {
		return fmt.Errorf("remove workspace: %w", err)
	}

	fmt.Fprintf(out, "workspace %q removed (any assigned projects were re-assigned to %q).\n",
		slug, orchestrator.DefaultWorkspaceSlug)
	fmt.Fprint(out, formatWorkspaceRemoveResult(slug, resp))
	return nil
}

// formatWorkspaceRemoveResult renders `workspace remove`'s post-deletion
// summary line(s) from the DELETE response. Sizing and deletion are two
// independent failure modes on WorkspaceRemoveResponse (HomeSizeError vs.
// HomeDeleteError — see that struct's doc comment), so this covers each
// combination rather than conflating a sizing hiccup into a single
// "home_delete_error" message.
//
// The identifier in each line is the HOME VOLUME's name, which is exactly
// what an operator passes to `docker volume rm` when the delete failed —
// the most likely reason being a job still running in that workspace,
// which the engine answers with a 409 that no force flag overrides.
//
// # Why "not deleted, and no error" is not silence
//
// HomeDeleted=false with both error fields empty has two meanings, and only
// one of them is benign:
//
//   - The daemon looked and there was no home volume — a workspace never
//     dispatched into. That is the init-on-first-dispatch contract ("a missing
//     home is not an error"), and it stays silent. What identifies this case is
//     that HomeVolume is still SET: dispatcher.WorkspaceHomeVolumeStore fills
//     WorkspaceHomeVolume.Volume unconditionally, existing volume or not, so a
//     daemon that ran the home path at all always names the volume it looked
//     for.
//   - Nothing was established at all. Either the daemon answered with an
//     older `home_path` key — which decodes into no field this CLI reads,
//     leaving HomeVolume "" — or it had no engine handle and skipped the home
//     block outright. Both are reachable: the CLI can drive a remote daemon,
//     and this repo has no API version handshake, so skew is a live
//     possibility. In both, the DB row is already gone while a volume holding
//     the workspace's harness credentials may well still be on the engine.
//
// Collapsing the second into "" was the defect: `workspace remove` printed
// "workspace ... removed." and stopped, which reads as a complete removal. The
// same reasoning covers HomeSizeError-with-no-deletion: the store only reports
// a size error alongside deleted=false when its VolumeInspect failed, i.e. when
// it never established the volume was there — so its VolumeRemove coming back
// clean proves nothing either, and calling that merely "size unknown"
// understates it.
//
// slug is taken so the actionable follow-up can be exact even when no volume
// name came back: dockerres.LabelWorkspaceHome carries the slug VERBATIM (the
// name is sanitized and not invertible, the label is not), so a label filter
// on it finds the workspace's home volume without the CLI needing to know
// the daemon's install id.
func formatWorkspaceRemoveResult(slug string, resp apiwire.WorkspaceRemoveResponse) string {
	return formatWorkspaceRemoveHomeResult(slug, resp) + formatWorkspaceRemoveInitScriptResult(resp)
}

// formatWorkspaceRemoveInitScriptResult renders the init.sh half of the
// summary — a warning when the deletion failed, and nothing at all
// otherwise.
//
// Silent on success for the same reason the home volume's "there was nothing
// to delete" case is silent: it is the expected outcome of the command that
// was just run, and a line per non-event trains an operator to skim past the
// one that matters.
//
// The failure is not skippable, though, and it is not the same kind of
// leftover as a stranded volume. Dispatch resolves a workspace's init.sh from
// the SLUG, so the file that survived this remove will be picked up by the
// next workspace created with the same name and run against its brand-new
// HOME volume. The row is already gone, which also rules out the obvious
// cleanup: `boid workspace unset-init-script` 404s on the missing row. The
// daemon puts its own path in the error, and the message says what the
// leftover will do rather than only that a file could not be removed.
func formatWorkspaceRemoveInitScriptResult(resp apiwire.WorkspaceRemoveResponse) string {
	if resp.InitScriptDeleteError == "" {
		return ""
	}
	return fmt.Sprintf("warning: the workspace's init.sh could not be deleted: %s\n"+
		"  it is on the DAEMON's filesystem (inside its state volume under the container deploy), and the workspace row is\n"+
		"  already gone, so `boid workspace unset-init-script` can no longer remove it — delete it there by hand.\n"+
		"  Left in place, a workspace re-created under this same name will inherit it and run it against its new HOME volume.\n",
		resp.InitScriptDeleteError)
}

// formatWorkspaceRemoveHomeResult renders the HOME volume half of the summary.
func formatWorkspaceRemoveHomeResult(slug string, resp apiwire.WorkspaceRemoveResponse) string {
	where := formatWorkspaceHomeVolumeRef(resp.HomeVolume)
	switch {
	// FIRST, ahead of every success case. An empty HomeVolume means the
	// responding daemon never looked at a volume at all, so nothing it
	// reports about the deletion can be a statement about one — including
	// HomeDeleted=true. An older daemon that finds a leftover host home
	// directory (workspace_home.go records that those survive) deletes THAT
	// and answers home_deleted=true with a home_path this CLI does not
	// decode; ordering this case after the success arm would turn that into
	// "home volume deleted", which is a claim the volume rewiring cannot
	// support. The workspace row is gone either way, so the only safe
	// report is that the volume is unconfirmed.
	case resp.HomeVolume == "":
		return fmt.Sprintf("warning: could not confirm the home volume was deleted: the daemon reported no home volume name"+
			" (a daemon older than the volume rewiring, or one with no engine handle)\n"+
			"  the workspace row is gone either way; check with `docker volume ls --filter label=%s=%s` and remove any match with `docker volume rm <name>`\n",
			dockerres.LabelWorkspaceHome, slug)
	case resp.HomeSizeError != "" && resp.HomeDeleteError != "":
		return fmt.Sprintf("warning: home volume size unknown and delete failed%s: size_error=%s delete_error=%s\n",
			where, resp.HomeSizeError, resp.HomeDeleteError)
	case resp.HomeDeleteError != "":
		return fmt.Sprintf("warning: home volume delete failed%s: %s\n", where, resp.HomeDeleteError)
	case resp.HomeDeleted:
		if resp.HomeSizeError != "" {
			return fmt.Sprintf("home volume deleted%s (size unknown: %s)\n", where, resp.HomeSizeError)
		}
		return fmt.Sprintf("home volume deleted%s (%s)\n", where, humanize.FormatBytes(resp.HomeBytes))
	// Everything below here: nothing was deleted, and the daemon reported no
	// deletion failure to explain why.
	case resp.HomeSizeError != "":
		return fmt.Sprintf("warning: could not confirm the home volume was deleted%s: %s\n"+
			"  the workspace row is gone either way; check with `docker volume ls --filter name=%s` and remove it with `docker volume rm %s` if it is still there\n",
			where, resp.HomeSizeError, resp.HomeVolume, resp.HomeVolume)
	default:
		// HomeVolume named, nothing failed, nothing deleted: the daemon
		// positively established there was no home volume to delete.
		return ""
	}
}

// formatWorkspaceHomeVolumeRef renders " (volume <name>)", or "" when the
// daemon reported no name.
//
// The empty case is CLI/daemon version skew, not an internal error: this repo
// has no API versioning, so an older daemon sends the old `home_path` /
// `path` key and the volume field decodes to "". Rendering a bare "()"
// there would look like a bug in the size lookup; omitting the parenthetical
// degrades to "the size, unattributed", which is what `boid gc`'s listing
// already does for an old daemon (see printWorkspaceHomes in cmd/gc.go).
//
// Omitting the identifier is NOT the same as omitting the outcome: see
// formatWorkspaceRemoveResult on why a missing name makes `workspace remove`
// warn rather than go quiet.
func formatWorkspaceHomeVolumeRef(volume string) string {
	if volume == "" {
		return ""
	}
	return fmt.Sprintf(" (volume %s)", volume)
}

// runWorkspaceExport lives in cmd/workspace_export.go: it composes the
// K8s-like envelope client-side from GET /api/workspaces/{slug} +
// GET /api/projects?workspace_id={slug}, and supports --all.

// runWorkspaceImportDeprecated replaces the old meta-format `workspace
// import`, retired because its round trip with
// GET /api/workspaces/{slug}/export (also retired) never actually worked
// end to end, on either side:
//
//   - an EMPTY workspace exported as "slug: default\n{}\n" — the marshal of
//     a zero-value WorkspaceMeta ("{}", every field omitempty) concatenated
//     with the spliced "slug:" line — which is not one valid yaml mapping,
//     so no decoder downstream of it, including this command's own, could
//     read it back;
//   - even a NON-empty export's top-level "slug:" key — spliced on so the
//     round trip needed no translation step — was rejected by THIS
//     command's own client-side orchestrator.DecodeWorkspaceMetaStrict
//     call, a bare-meta decoder that has never accepted a "slug" field.
//
// Patching one half would not have fixed the other, so both were retired in
// favor of the boid.dev/v1 envelope format `boid workspace export`/`apply`
// already use successfully — the one round trip in this family that is
// actually exercised end to end (see cmd/workspace_apply_test.go).
//
// This always fails, pointing at the two paths that cover what `workspace
// import` did depending on the file's shape: `apply -f` for a boid.dev/v1
// envelope document, or `create`/`edit --from-file` for a bare workspace-meta
// document (host_commands:/env:/... at the top level, no apiVersion/kind —
// e.g. the shadow yaml `boid project migrate` writes).
//
// That pair does NOT losslessly replace `--mode replace`'s one thing: a
// single command that creates-or-overwrites without the caller needing to
// know beforehand whether the slug exists (Workspaces.Save is a true
// upsert). cmd/project_migrate.go's own manual-fallback guidance
// (shadowFileApplyHintBothCases) has call sites reached specifically because
// the daemon could not be asked whether the slug exists at all — those now
// have to name BOTH `create` and `edit` and let the operator pick. This gap
// is accepted as a minor, guidance-text-only regression — every such site
// is printed only when the daemon is being reached OFFLINE by a human
// reading terminal output, not a codepath anything else depends on.
func runWorkspaceImportDeprecated(cmd *cobra.Command, args []string) error {
	msg := `boid workspace import は廃止されました (meta 形式の export/import が
双方向とも壊れていたため、envelope 形式に一本化)。 ファイルの形式に応じて
次のいずれかを使ってください:

  apiVersion/kind 付きの envelope 文書 (boid workspace export の出力):
    boid workspace apply -f <file>

  host_commands: / env: などが top-level にある bare な workspace meta 文書
  (boid project migrate が書き出す shadow yaml など):
    boid workspace create <slug> --from-file <file>   (新規 workspace)
    boid workspace edit   <slug> --from-file <file>   (既存 workspace)
`
	fmt.Fprint(cmd.ErrOrStderr(), msg)
	return fmt.Errorf("boid workspace import is deprecated; see the guidance above")
}

// formatStringSlice formats a slice for display: "(none)" when empty, or comma-joined.
func formatStringSlice(ss []string) string {
	if len(ss) == 0 {
		return "(none)"
	}
	return strings.Join(ss, ", ")
}

// formatWorkspaceHomeSize renders a *apiwire.WorkspaceHomeSize as a single
// human-readable line for `boid workspace show`: a normal size + the home's
// docker volume name, "0 B (未作成: ...)" for a workspace never dispatched
// into, or "?" when the daemon could not determine the size — never
// failing the whole command over a size lookup.
//
// formatWorkspaceHomeVolumeRef explains what an empty volume name means and
// why it is rendered as an omission rather than as "()".
func formatWorkspaceHomeSize(h *apiwire.WorkspaceHomeSize) string {
	switch {
	case h.SizeError != "":
		return fmt.Sprintf("home size: ?%s", formatWorkspaceHomeVolumeRef(h.Volume))
	case !h.Exists:
		if h.Volume == "" {
			return "home size: 0 B (未作成)"
		}
		return fmt.Sprintf("home size: 0 B (未作成: volume %s)", h.Volume)
	default:
		return fmt.Sprintf("home size: %s%s", humanize.FormatBytes(h.Bytes), formatWorkspaceHomeVolumeRef(h.Volume))
	}
}
