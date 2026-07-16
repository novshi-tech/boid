package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/config"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/skills"
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
	// meta wholesale (decision 14 — no individual field flags).
	workspaceEditFromFile string
	// workspaceEditForce skips the automatic If-Match revision check
	// (decision 17), for a deliberate last-write-wins edit.
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

// workspaceConfigureExecFn runs the sandbox runner as a child process (fork+wait) and
// waits for it to complete. It is a package-level variable so tests can
// override it to intercept the launch without actually running a sandbox.
//
// Unlike syscall.Exec, this does NOT replace the current process — the caller
// regains control after the child exits, allowing post-run scanning logic to
// execute.
var workspaceConfigureExecFn = func(argv0 string, argv []string, envv []string) error {
	cmd := exec.Command(argv0, argv[1:]...)
	cmd.Env = envv
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// workspaceConfigureCmd launches a sandboxed agent session (ProfileInit) that
// reads the assigned projects and writes a workspace.yaml via the
// boid-sandbox-configure skill (workspace-configure mode). After the sandbox
// exits, the generated yaml is scanned for secrets; if any are found the
// original file is restored from backup and an error is returned.
//
// Unlike boid kit init, the *host-side* CLI requires the daemon (it fetches
// the assigned project list via GET /api/projects before dispatch, and calls
// reloadProjects() after) and therefore does NOT carry annotationSkipAutostart.
// The sandboxed skill itself no longer talks to the daemon — see the
// ServerSocket comment on the SandboxRuntimeInfo below.
var workspaceConfigureCmd = &cobra.Command{
	Use:   "configure <slug>",
	Short: "Configure a workspace via interactive agent session (boid-sandbox-configure skill, workspace-configure mode)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkspaceConfigure(cmd, args)
	},
}

var workspaceRemoveCmd = &cobra.Command{
	Use:   "remove <slug>",
	Short: "Remove a workspace (DELETE /api/workspaces/{slug}; assigned projects are re-assigned to default)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceRemove,
}

// workspaceExportOutput is the optional --output flag value for `workspace
// export`: a file path to write the exported yaml to, instead of stdout.
var workspaceExportOutput string

var workspaceExportCmd = &cobra.Command{
	Use:   "export <slug>",
	Short: "Export a workspace's definition as yaml (GET /api/workspaces/{slug}/export)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceExport,
}

var (
	// workspaceImportMode is the --mode flag value for `workspace import`:
	// "create-only" (default, safe — never overwrites an existing slug) or
	// "replace" (explicit upsert, last-write-wins).
	workspaceImportMode string
	// workspaceImportForce is a shorthand for --mode replace.
	workspaceImportForce bool
	// workspaceImportSlug is the optional --slug flag value for `workspace
	// import`: the export endpoint's yaml body carries no "slug" key (see
	// WorkspaceHandler.Export's doc comment), so import must recover a
	// target slug from somewhere else — either this flag, or (if omitted)
	// the import file's basename with its extension stripped.
	workspaceImportSlug string
)

var workspaceImportCmd = &cobra.Command{
	Use:   "import <file>",
	Short: "Import a workspace definition from yaml (POST /api/workspaces/import?mode=<create-only|replace>)",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceImport,
}

func init() {
	// Every workspace subcommand talks to the daemon's HTTP API (including
	// `configure`, which dispatches a sandbox job through it) — all
	// scopeRemote (docs/plans/workspace-db-consolidation.md decision 18).
	for _, c := range []*cobra.Command{
		workspaceListCmd, workspaceShowCmd, workspaceCreateCmd, workspaceEditCmd,
		workspaceAssignCmd, workspaceClearCmd, workspaceConfigureCmd, workspaceRemoveCmd,
		workspaceExportCmd, workspaceImportCmd,
	} {
		c.Annotations = map[string]string{scopeAnnotationKey: scopeRemote}
	}

	workspaceCreateCmd.Flags().StringVar(&workspaceCreateFromFile, "from-file", "", "yaml file describing the workspace meta (optional; omit to create a blank workspace)")
	workspaceEditCmd.Flags().StringVar(&workspaceEditFromFile, "from-file", "", "yaml file with the new workspace meta (required)")
	workspaceEditCmd.Flags().BoolVar(&workspaceEditForce, "force", false, "skip the If-Match revision check (last-write-wins)")
	workspaceExportCmd.Flags().StringVar(&workspaceExportOutput, "output", "", "file path to write the exported yaml to (default: stdout)")
	workspaceImportCmd.Flags().StringVar(&workspaceImportMode, "mode", "create-only", "import mode: create-only (default, 409 on an existing slug) or replace (upsert)")
	workspaceImportCmd.Flags().BoolVar(&workspaceImportForce, "force", false, "shorthand for --mode replace")
	workspaceImportCmd.Flags().StringVar(&workspaceImportSlug, "slug", "", "target workspace slug (default: the import file's basename, extension stripped — the export body itself carries no slug)")

	workspaceCmd.AddCommand(
		workspaceListCmd,
		workspaceShowCmd,
		workspaceCreateCmd,
		workspaceEditCmd,
		workspaceAssignCmd,
		workspaceClearCmd,
		workspaceConfigureCmd,
		workspaceRemoveCmd,
		workspaceExportCmd,
		workspaceImportCmd,
	)
	rootCmd.AddCommand(workspaceCmd)
}

// runWorkspaceList lists every workspace via GET /api/workspaces
// (docs/plans/workspace-db-consolidation.md PR4 Step H): the workspaces
// table is now the single source of truth, so unlike the pre-PR4 "yaml dir
// scan + DB scan union" this needs no local filesystem read at all — an
// empty workspace (no assigned projects) is already included in the API
// response (Step B's ListWorkspaces rewrite).
func runWorkspaceList(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

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
	Projects []*orchestrator.Project     `json:"projects"`
}

// runWorkspaceShow shows a workspace's definition (GET /api/workspaces/{slug})
// alongside its assigned projects (GET /api/projects?workspace_id=<slug>,
// unaffected by PR4 — kept so the project listing can still show each
// project's WorkDir, which the workspace endpoint's AssignedProjects
// (project ids only) does not carry). Step H removes the direct
// orchestrator.WorkspaceStore.Load call this command used before cutover —
// a workspace that only exists as a local workspace.yaml (never assigned or
// `boid workspace create`d) now 404s here, matching the DB-is-authority
// contract PR3/PR4 established.
func runWorkspaceShow(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.NewUnixClient(client.DefaultSocketPath())

	var detail api.WorkspaceDetail
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
		Projects: projects,
	}

	return renderOutput(cmd, view, func() error {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Workspace: %s\n", slug)
		if view.Revision != "" {
			fmt.Fprintf(out, "revision: %s\n", view.Revision)
		}

		if meta := view.Meta; meta != nil {
			if len(meta.Kits) > 0 {
				fmt.Fprintf(out, "kits (legacy): %s\n", formatStringSlice(meta.Kits))
			}
			fmt.Fprintf(out, "host_commands: %s\n", formatStringSlice(meta.HostCommands))
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

// runWorkspaceCreate creates a new workspace via POST /api/workspaces
// (docs/plans/workspace-db-consolidation.md PR4 Step H). --from-file is
// optional: omitted, it creates a blank workspace; given, its yaml content
// is merged with the target slug into the single combined body the create
// endpoint expects (Step C: "slug は body 内").
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

		// MINOR 1 (codex review round 3, docs/plans/workspace-db-consolidation.md):
		// validate --from-file with the same strict (multi-document-rejecting)
		// decoder the server uses, before buildWorkspaceCreateBody's loose
		// map[string]any merge below gets a chance to silently drop a second
		// "---"-delimited document (see orchestrator.DecodeWorkspaceMetaStrict's
		// doc comment for why a plain yaml.Unmarshal never surfaces that as an
		// error). This is a client-side fail-fast check only — the server
		// performs the same validation again on the constructed body.
		if _, err := orchestrator.DecodeWorkspaceMetaStrict(metaYAML); err != nil {
			return fmt.Errorf("validate --from-file: %w", err)
		}
	}

	body, err := buildWorkspaceCreateBody(slug, metaYAML)
	if err != nil {
		return fmt.Errorf("build create request: %w", err)
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	var detail api.WorkspaceDetail
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
// The strict decoder is invoked on metaYAML first (codex 4th pass, minor):
// the server's DecodeWorkspaceCreateStrict runs against the fully-marshalled
// body below, but yaml.Unmarshal only reads the *first* document — so a
// caller passing a two-document file would silently drop the second one
// before the server ever sees it, defeating minor 2's multi-document
// reject from PR4's earlier pass. Validating metaYAML up-front (which
// itself uses a strict decoder that rejects trailing documents and unknown
// nested fields) closes that hole and gives configure/create both the
// same fail-fast behaviour edit already had via server-side validation.
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
// PUT /api/workspaces/{slug} (docs/plans/workspace-db-consolidation.md PR4
// Step H, decision 14: --from-file only, no individual field flags).
// Unless --force is set, the current revision is fetched first (a plain
// GET) and sent back as If-Match — the CLI attaches the ETag automatically
// so the common case ("edit what I just saw") never needs the caller to
// juggle revisions by hand; --force skips this for a deliberate
// last-write-wins edit (decision 17).
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

	// MINOR 1 (codex review round 3, docs/plans/workspace-db-consolidation.md):
	// fail fast on a multi-document --from-file before making any daemon
	// call. The server (PUT /api/workspaces/{slug}) already runs this exact
	// same check on the raw body this function forwards verbatim, so this is
	// a client-side convenience — it saves a round trip (including the
	// automatic revision GET below) and reports the failure without needing
	// the daemon reachable at all.
	if _, err := orchestrator.DecodeWorkspaceMetaStrict(data); err != nil {
		return fmt.Errorf("validate --from-file: %w", err)
	}

	c := client.NewUnixClient(client.DefaultSocketPath())

	var ifMatch string
	if !workspaceEditForce {
		var current api.WorkspaceDetail
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

	var detail api.WorkspaceDetail
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
// (PUT /api/projects/{id}/workspace). PR4 reinstates the daemon-side
// existence check this endpoint used to skip (Step J, MAJOR 5), so unlike
// the pre-PR4 "get-or-create" semantics, assigning to an unknown slug now
// 404s — unless a local workspace.yaml for that slug already exists (e.g.
// written by `boid workspace configure`, which never creates a DB row
// itself), in which case ensureWorkspaceExistsForAssign auto-creates the DB
// row from it first so the existing "drop a yaml, then assign" flow keeps
// working end to end.
func runWorkspaceAssign(cmd *cobra.Command, args []string) error {
	// CLI entry-point validation per plan (3-layer defense). Early error gives
	// a better UX than a 400 from the daemon.
	slug := args[1]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.NewUnixClient(client.DefaultSocketPath())

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

// ensureWorkspaceExistsForAssign implements `boid workspace assign`'s
// auto-create pre-check (docs/plans/workspace-db-consolidation.md PR4 Step
// H): if slug already has a workspaces row, this is a no-op. Otherwise, if
// a legacy local workspace.yaml exists for slug (dropped by hand, by an e2e
// scenario, or by `boid workspace configure` — which only ever writes the
// local yaml, never a DB row), its content is POSTed to the daemon so the
// assignment's reinstated existence check (Step J) can succeed. If neither
// exists, this is a silent no-op: the subsequent assign call surfaces
// whatever the real outcome is (a plain 404 for a genuinely unknown slug),
// exactly like the pre-PR4 behavior for that case.
//
// MINOR 3-b (codex review): a local workspace.yaml read failure other than
// "file does not exist" (a parse error, or a permission error) is no longer
// silently swallowed and papered over as "no local yaml either" — that used
// to make a real config or filesystem problem indistinguishable from the
// legitimate "nothing to auto-create from" case, surfacing only as a
// confusing 404 from the *subsequent* assign call instead of the actual
// cause. Only os.ErrNotExist now falls through to that silent path; anything
// else is returned so the CLI reports the real error.
func ensureWorkspaceExistsForAssign(c *client.Client, slug string, out io.Writer) error {
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &api.WorkspaceDetail{}); err == nil {
		return nil // already has a DB row.
	}

	store := orchestrator.NewWorkspaceStore("")
	meta, err := store.Load(slug)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no local yaml either — let the assign call's own error surface.
		}
		return fmt.Errorf("read local workspace.yaml %q for auto-create: %w", slug, err)
	}

	// MINOR 1 (codex review round 3, docs/plans/workspace-db-consolidation.md):
	// validate the raw local yaml with the same strict (multi-document-
	// rejecting) decoder the server uses, before re-marshaling `meta` below.
	// store.Load's plain yaml.Unmarshal above already silently drops a second
	// "---"-delimited document — the server's own strict reject
	// (DecodeWorkspaceMetaStrict, wired into POST /api/workspaces) would then
	// never get a chance to see it, since only the already-truncated struct
	// ever gets re-marshaled and POSTed by postWorkspaceCreateBestEffort
	// below. A read failure here is not itself an error: store.Load already
	// succeeded reading the same file moments ago, so this is best-effort.
	if wsDir, dirErr := orchestrator.DefaultWorkspaceDir(); dirErr == nil {
		if raw, readErr := os.ReadFile(filepath.Join(wsDir, slug+".yaml")); readErr == nil {
			if _, strictErr := orchestrator.DecodeWorkspaceMetaStrict(raw); strictErr != nil {
				return fmt.Errorf("validate local workspace.yaml %q for auto-create: %w", slug, strictErr)
			}
		}
	}

	data, err := yaml.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal local workspace.yaml for auto-create: %w", err)
	}
	postWorkspaceCreateBestEffort(c, slug, data, out, "from local workspace.yaml")
	return nil
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
// assign`) and ensureWorkspaceExistsGetOrCreate (`boid project add/init
// --workspace`, MAJOR 4 codex review) — the two differ only in what
// metaYAML (if any) they create from and how they describe the source in
// the printed note.
func postWorkspaceCreateBestEffort(c *client.Client, slug string, metaYAML []byte, out io.Writer, sourceDescription string) {
	body, err := buildWorkspaceCreateBody(slug, metaYAML)
	if err != nil {
		fmt.Fprintf(out, "note: build auto-create request for workspace %q failed: %v\n", slug, err)
		return
	}
	if err := c.DoWithContentType("POST", "/api/workspaces", "application/yaml", body, &api.WorkspaceDetail{}); err != nil {
		fmt.Fprintf(out, "note: auto-create workspace %q %s failed: %v\n", slug, sourceDescription, err)
		return
	}
	fmt.Fprintf(out, "workspace %q auto-created %s\n", slug, sourceDescription)
}

// ensureWorkspaceExistsGetOrCreate implements `boid project add/init
// --workspace`'s get-or-create contract (MAJOR 4, codex review,
// docs/plans/workspace-db-consolidation.md): an empty workspace is created
// unconditionally when slug has no DB row yet — unlike `boid workspace
// assign`'s auto-create (ensureWorkspaceExistsForAssign), which only fires
// for a slug with a pre-existing legacy workspace.yaml and otherwise
// silently no-ops so a genuinely unknown slug still 404s on assign. `project
// add`/`project init`'s own long-standing docstring already promised this
// get-or-create behavior ("DB row is created even for unknown slug"), but
// it had never actually been implemented — the caller (assignProjectWorkspace)
// only ever called the assign PUT, so an unknown slug 404'd there instead,
// leaving `project add` itself already-succeeded (partial success).
func ensureWorkspaceExistsGetOrCreate(c *client.Client, slug string, out io.Writer) error {
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &api.WorkspaceDetail{}); err == nil {
		return nil // already exists.
	}
	postWorkspaceCreateBestEffort(c, slug, nil, out, "(empty, get-or-create)")
	return nil
}

func runWorkspaceClear(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

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

func runWorkspaceConfigure(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	c := client.NewUnixClient(client.DefaultSocketPath())

	// 1. Resolve default harness (workspace configure requires one; kit init sets
	//    it on first run, so by the time configure is called it should be set).
	harness, err := config.DefaultHarness()
	if err != nil {
		if errors.Is(err, config.ErrDefaultHarnessNotSet) {
			return fmt.Errorf("default harness not configured — run `boid kit init` first to set it")
		}
		return fmt.Errorf("resolve default harness: %w", err)
	}
	fmt.Fprintf(out, "default harness: %s\n", harness)

	// 2. Deploy embedded skills to the host (idempotent) so the adapter can
	//    bind-mount them into the sandbox.
	skillsDir := defaultSkillsDir()
	if err := skills.DeployAll(skillsDir); err != nil {
		return fmt.Errorf("deploy skills: %w", err)
	}

	// 3. Fetch assigned projects from the daemon to gather their WorkDirs for
	//    read-only bind mounts so the skill can read package.json / go.mod etc.
	var projects []*orchestrator.Project
	if err := c.Do("GET", "/api/projects?workspace_id="+slug, nil, &projects); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}

	fmt.Fprintf(out, "Workspace: %s\n", slug)
	if len(projects) == 0 {
		fmt.Fprintln(out, "  (no projects assigned — use `boid workspace assign <project> "+slug+"` first)")
	} else {
		fmt.Fprintf(out, "Assigned projects (%d):\n", len(projects))
		for _, p := range projects {
			fmt.Fprintf(out, "  %s  %s\n", p.ID, p.WorkDir)
		}
	}
	fmt.Fprintln(out)

	// 4. Determine the workspace.yaml path and ensure the parent dir exists.
	store := orchestrator.NewWorkspaceStore("")
	wsDir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		return fmt.Errorf("workspace dir: %w", err)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return fmt.Errorf("create workspace dir: %w", err)
	}
	wsYAML := filepath.Join(wsDir, slug+".yaml")

	// 5. Backup existing workspace.yaml (if any) and ensure the file exists
	//    (touch) with mode 0o600 so the post-sandbox secret scan always has a
	//    file to read and so the agent's umask cannot widen the mode. The
	//    binding itself is now on the parent dir (WritableDirs), not the file
	//    — Write/Edit's atomic rename needs the parent writable, and a
	//    single-file IsFile bind would block it with EROFS.
	bakPath, err := backupWorkspaceYAML(wsYAML)
	if err != nil {
		return fmt.Errorf("backup workspace.yaml: %w", err)
	}

	// 6. Resolve the boid binary path (runner-outer is re-exec'd via this path).
	boidBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve boid binary: %w", err)
	}

	// 7. Collect WorkDirs from the assigned projects as read-only binds so the
	//    skill can read package.json / go.mod / hook scripts without write access.
	var roBinds []string
	for _, p := range projects {
		if p.WorkDir != "" {
			roBinds = append(roBinds, p.WorkDir)
		}
	}

	// 7.5. Encode the assigned project list (id + work_dir only) as JSON so
	//      the sandboxed skill can read it from $BOID_WORKSPACE_PROJECTS
	//      instead of calling `boid workspace show` — the sandbox has no
	//      daemon socket (see ServerSocket below).
	projectsJSON, err := projectsEnvJSON(projects)
	if err != nil {
		return fmt.Errorf("encode workspace projects: %w", err)
	}

	// 8. Build the JobSpec via BuildInitJobSpec.
	jobID := fmt.Sprintf("workspace-configure-%s", randomJobSuffix())
	// The parent dir (~/.config/boid/workspaces/) is bind-mounted rw rather
	// than just the target <slug>.yaml. A single-file IsFile bind would block
	// the atomic write pattern (write to <name>.tmp.<pid>.<rand> in the parent
	// dir, then rename) used by harness-side file editors with EROFS, which
	// pinned the skill to shell-only writes and broke harness-agnosticism.
	// The blast radius is contained: this dir only holds per-slug workspace
	// yamls, and the post-sandbox secret scan still vetoes any leaked secret.
	spec := dispatcher.BuildInitJobSpec(dispatcher.InitJobInput{
		Profile:       sandbox.ProfileInit,
		WritableDirs:  []string{wsDir},
		ReadOnlyBinds: roBinds,
		Argv:          []string{"boid-workspace-configure"},
		DisplayName:   "boid workspace configure " + slug,
		HarnessType:   harness,
		Env: map[string]string{
			"BOID_WORKSPACE_SLUG": slug,
			// project id + work_dir list, injected so the skill never needs
			// to reach the daemon from inside the sandbox (see ServerSocket
			// below). Populated even when empty ("[]") so the skill can
			// distinguish "no projects assigned" from "env missing".
			"BOID_WORKSPACE_PROJECTS": projectsJSON,
		},
		// Bootstrap prompt — matches a trigger phrase from the
		// boid-sandbox-configure SKILL.md frontmatter (workspace-configure
		// mode). Embedding the slug lets the skill skip the "which
		// workspace" question and dive straight into the project scan,
		// mirroring how `boid kit init` kicks its own mode on launch.
		Instruction: fmt.Sprintf("workspace %q の boid workspace configure を実行して", slug),
	})

	// 9. Build the SandboxRuntimeInfo.
	//    ServerSocket is intentionally empty: the sandboxed skill no longer
	//    calls any daemon API. The only in-sandbox daemon dependency was
	//    `boid workspace show` for the assigned project list, which is now
	//    injected via BOID_WORKSPACE_PROJECTS above (this same host-side
	//    runWorkspaceConfigure call already fetched it at step 3, before
	//    dispatch). `boid kit list` never needed the daemon — it reads
	//    ~/.local/share/boid/kits/ directly, which is already visible via
	//    ProfileInit's read-only host-root rbind. Leaving this empty is a
	//    minimum-privilege cut: it closes the "compromised agent spawns a
	//    task/session and fires a forged kit right here" self-launch path.
	//
	//    ProxyPort must still be wired the same as `boid kit init` — ProfileInit
	//    sandboxes can only egress through the daemon proxy port; without
	//    HTTPS_PROXY, AI agent harnesses fail with FailedToOpenSocket and
	//    nothing reaches api.anthropic.com.
	rt := dispatcher.SandboxRuntimeInfo{
		JobID:        jobID,
		BoidBinary:   boidBinary,
		ServerSocket: "", // daemon not required inside the sandbox
		ProxyPort:    resolveDaemonProxyPort(out),
		Foreground:   true,
	}

	sbSpec, err := dispatcher.BuildSandboxSpec(spec, rt)
	if err != nil {
		return restoreWorkspaceYAMLOrJoin(wsYAML, bakPath, fmt.Errorf("build sandbox spec: %w", err))
	}

	sb, err := dispatcher.NewSandboxPreparer().PrepareSandbox(sbSpec)
	if err != nil {
		return restoreWorkspaceYAMLOrJoin(wsYAML, bakPath, fmt.Errorf("prepare sandbox: %w", err))
	}
	if sb == nil || sb.SpecPath == "" {
		return restoreWorkspaceYAMLOrJoin(wsYAML, bakPath, fmt.Errorf("prepare sandbox: missing spec path"))
	}

	// 10. Run the runner-outer as a child process and wait for completion.
	runnerArgs := []string{boidBinary, "runner-outer", "--spec", sb.SpecPath, "--state", sb.StatePath}
	if execErr := workspaceConfigureExecFn(boidBinary, runnerArgs, os.Environ()); execErr != nil {
		// Sandbox failed — restore backup.
		return restoreWorkspaceYAMLOrJoin(wsYAML, bakPath, execErr)
	}

	// 11. Scan the written workspace.yaml for secrets.
	//     On finding(s): restore backup + return error.
	//     On clean: print summary and fall through to the DB sync below.
	//     The backup is deliberately NOT deleted here — see MAJOR 2 below.
	if err := scanWorkspaceYAML(wsYAML, bakPath, out, store, slug); err != nil {
		return err
	}

	// 11.5. Push the generated workspace.yaml to the DB-backed workspace
	// store, finalizing the backup's lifecycle based on the outcome — see
	// syncWorkspaceYAMLAndFinalizeBackup's doc comment (MAJOR 5 and MAJOR 2,
	// codex review rounds 1 and 2, docs/plans/workspace-db-consolidation.md).
	if err := syncWorkspaceYAMLAndFinalizeBackup(c, slug, wsYAML, bakPath); err != nil {
		return err
	}

	// 12. Reload projects so the daemon picks up kit changes in workspace.yaml.
	reloadProjects()
	return nil
}

// syncWorkspaceYAMLAndFinalizeBackup reads wsYAML's freshly (re)generated
// content and pushes it to the daemon's DB-backed workspace store via
// syncWorkspaceYAMLToDB (docs/plans/workspace-db-consolidation.md MAJOR 5,
// codex review: PR3/PR4 made the workspaces table authoritative and removed
// the yaml-fallback Load path, but `workspace configure` used to only ever
// write the local yaml file, making it a silent no-op post-cutover).
//
// MAJOR 2 (codex review round 2): bakPath's lifecycle is decided here, after
// the sync attempt, not before it. Before this fix, scanWorkspaceYAML deleted
// bakPath itself right after the secret scan passed — i.e. before this sync
// ever ran. A subsequent sync failure (daemon unreachable, strict parse
// rejecting an unknown host_commands reference, etc.) then left the operator
// with neither the pre-configure yaml (backup already gone) nor a working
// synced one, silently diverging the local shadow yaml from the DB. Now: a
// sync failure restores wsYAML from bakPath (or removes wsYAML entirely when
// bakPath is empty — i.e. there was no pre-configure yaml to begin with,
// mirroring restoreWorkspaceYAML's own empty-bakPath contract); a sync
// success deletes bakPath, since it has served its purpose. Extracted out of
// runWorkspaceConfigure so this ordering is independently unit-testable
// without needing a full sandbox run.
func syncWorkspaceYAMLAndFinalizeBackup(c *client.Client, slug, wsYAML, bakPath string) error {
	wsData, err := os.ReadFile(wsYAML)
	if err != nil {
		return restoreWorkspaceYAMLOrJoin(wsYAML, bakPath, fmt.Errorf("read generated workspace.yaml for DB sync: %w", err))
	}
	if err := syncWorkspaceYAMLToDB(c, slug, wsData); err != nil {
		return restoreWorkspaceYAMLOrJoin(wsYAML, bakPath, fmt.Errorf("sync workspace.yaml to DB: %w", err))
	}

	// Sync succeeded — the backup has served its purpose.
	if bakPath != "" {
		_ = os.Remove(bakPath)
	}
	return nil
}

// syncWorkspaceYAMLToDB pushes wsYAML's content (the freshly (re)generated
// local workspace.yaml) to the daemon's DB-backed workspace store (MAJOR 5,
// codex review, docs/plans/workspace-db-consolidation.md): GET checks
// whether slug already has a DB row — missing means POST /api/workspaces
// (create); present means PUT /api/workspaces/{slug}?force=true, a
// deliberate destructive whole-document replace. force=true (skipping
// If-Match) is intentional here, not a shortcut: `workspace configure`
// regenerates the entire file from scratch inside the sandbox, so there is
// no meaningful prior revision for this CLI invocation to have captured
// and round-trip as an ETag the way `workspace edit` does.
//
// MINOR 1 (codex review round 2): the existence check uses GetRaw (not Do)
// specifically so a 404 can be distinguished from every other outcome. Do
// collapses any GET failure — a 500, a connection failure, a response
// decode error — into a single generic error, and this function used to
// treat that whole bucket as "no DB row yet" and fall through to create.
// That silently masked real daemon-side problems (and, worse, could paper
// over a genuine 500 by attempting a POST that itself might succeed against
// stale server state). Only a literal 404 now triggers the create path;
// anything else propagates as an error without ever reaching POST.
func syncWorkspaceYAMLToDB(c *client.Client, slug string, wsYAML []byte) error {
	getStatus, getBody, getErr := c.GetRaw("/api/workspaces/" + slug)
	if getErr != nil {
		return fmt.Errorf("check workspace %q existence in DB: %w", slug, getErr)
	}
	if getStatus != http.StatusOK && getStatus != http.StatusNotFound {
		return fmt.Errorf("check workspace %q existence in DB: %s", slug, formatWorkspaceAPIError(getStatus, getBody))
	}

	if getStatus == http.StatusNotFound {
		// No DB row yet — create it.
		body, buildErr := buildWorkspaceCreateBody(slug, wsYAML)
		if buildErr != nil {
			return fmt.Errorf("build workspace create body: %w", buildErr)
		}
		if err := c.DoWithContentType("POST", "/api/workspaces", "application/yaml", body, &api.WorkspaceDetail{}); err != nil {
			return fmt.Errorf("create workspace %q in DB: %w", slug, err)
		}
		return nil
	}

	statusCode, respBody, err := c.PutRawWithIfMatch("/api/workspaces/"+slug+"?force=true", "application/yaml", wsYAML, "")
	if err != nil {
		return fmt.Errorf("update workspace %q in DB: %w", slug, err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("update workspace %q in DB: %s", slug, formatWorkspaceAPIError(statusCode, respBody))
	}
	return nil
}

// backupWorkspaceYAML ensures the workspace yaml file exists (creating it if
// absent with mode 0o600) and writes a backup copy to <path>.bak.<unixtime>
// if the file already had content. Returns the backup path (may be empty if
// the original did not exist) and any error.
func backupWorkspaceYAML(wsYAML string) (bakPath string, err error) {
	existing, readErr := os.ReadFile(wsYAML)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("read workspace.yaml: %w", readErr)
	}

	if !errors.Is(readErr, os.ErrNotExist) && len(existing) > 0 {
		// File exists with content — create a backup.
		bakPath = fmt.Sprintf("%s.bak.%d", wsYAML, os.Getpid())
		if err := os.WriteFile(bakPath, existing, 0o600); err != nil {
			return "", fmt.Errorf("write backup: %w", err)
		}
	}

	// Touch the file (create empty or leave existing in place — truncate only
	// if it did not exist yet so the sandbox can overwrite via RW bind).
	if errors.Is(readErr, os.ErrNotExist) {
		if err := os.WriteFile(wsYAML, []byte{}, 0o600); err != nil {
			return bakPath, fmt.Errorf("touch workspace.yaml: %w", err)
		}
	}
	return bakPath, nil
}

// restoreWorkspaceYAML restores wsYAML from bakPath (if a backup exists) or
// removes wsYAML if there was no backup (the file was newly created by
// touch). The touch-created-file removal path remains best-effort (there is
// no caller content to lose there), but a failure restoring from an actual
// backup is now returned rather than swallowed.
//
// MAJOR 2 (codex review round 3, docs/plans/workspace-db-consolidation.md):
// before this fix, os.WriteFile's error was ignored and bakPath was removed
// unconditionally right after — a write failure (disk full, permission
// error, etc.) silently destroyed the only surviving copy of the
// pre-configure workspace.yaml, with no way to recover it. bakPath is now
// only removed once the write-back has actually succeeded.
func restoreWorkspaceYAML(wsYAML, bakPath string) error {
	if bakPath == "" {
		// No pre-existing content — remove the touch-created file.
		_ = os.Remove(wsYAML)
		return nil
	}
	bak, err := os.ReadFile(bakPath)
	if err != nil {
		return fmt.Errorf("read backup %q: %w", bakPath, err)
	}
	if err := os.WriteFile(wsYAML, bak, 0o600); err != nil {
		return fmt.Errorf("write restored workspace.yaml: %w", err)
	}
	_ = os.Remove(bakPath)
	return nil
}

// restoreWorkspaceYAMLOrJoin restores wsYAML from bakPath and folds any
// restore failure into origErr via errors.Join (MAJOR 2, codex review round
// 3, docs/plans/workspace-db-consolidation.md): a restore failure must never
// be silently swallowed while origErr alone is reported, since a failed
// restore on top of an already-failing operation means bakPath is now the
// only surviving copy of the pre-configure workspace.yaml — the operator
// needs to know that explicitly, not just that the original operation
// failed. When the restore succeeds, origErr is returned unchanged.
func restoreWorkspaceYAMLOrJoin(wsYAML, bakPath string, origErr error) error {
	if restoreErr := restoreWorkspaceYAML(wsYAML, bakPath); restoreErr != nil {
		return errors.Join(origErr, fmt.Errorf("restore backup: %w", restoreErr))
	}
	return origErr
}

// workspaceProjectEnv is the minimal per-project shape injected into the
// workspace-configure sandbox as JSON via $BOID_WORKSPACE_PROJECTS. Only ID
// and WorkDir are carried — the sandbox has no business seeing the rest of
// orchestrator.Project (ProjectMeta, timestamps, etc.).
type workspaceProjectEnv struct {
	ID      string `json:"id"`
	WorkDir string `json:"work_dir"`
}

// projectsEnvJSON marshals the assigned projects into the compact JSON array
// the boid-sandbox-configure skill (workspace-configure mode) reads from
// $BOID_WORKSPACE_PROJECTS in place of a `boid workspace show` daemon call.
// An empty/nil slice marshals to "[]" (never "null") so the skill can tell
// "no projects assigned" apart from "env var missing".
func projectsEnvJSON(projects []*orchestrator.Project) (string, error) {
	entries := make([]workspaceProjectEnv, 0, len(projects))
	for _, p := range projects {
		entries = append(entries, workspaceProjectEnv{ID: p.ID, WorkDir: p.WorkDir})
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal workspace projects: %w", err)
	}
	return string(b), nil
}

// scanWorkspaceYAML scans the single workspace.yaml file for secrets.
// On findings: restores backup from bakPath and returns an error.
// On clean: prints a kit summary and returns nil.
//
// MAJOR 2 (codex review round 2, docs/plans/workspace-db-consolidation.md):
// this function deliberately does NOT delete bakPath on the clean path
// anymore — it used to, but that ran before runWorkspaceConfigure's
// subsequent DB sync step, so a sync failure (strict parse rejecting an
// unknown host_commands reference, daemon unreachable, etc.) left the
// operator with no backup to restore from, silently diverging the local
// shadow yaml from the DB. The caller (runWorkspaceConfigure) now owns
// deleting bakPath, only after the DB sync itself succeeds.
func scanWorkspaceYAML(wsYAML, bakPath string, out interface{ Write([]byte) (int, error) }, store *orchestrator.WorkspaceStore, slug string) error {
	findings, err := orchestrator.ScanSecretsFile(wsYAML)
	if err != nil {
		return restoreWorkspaceYAMLOrJoin(wsYAML, bakPath, fmt.Errorf("secret scan: %w", err))
	}

	if len(findings) > 0 {
		var sb strings.Builder
		sb.WriteString("secret scan: suspicious values detected in generated workspace.yaml — rolled back\n")
		for _, f := range findings {
			sb.WriteString("  ")
			sb.WriteString(f.String())
			sb.WriteString("\n")
		}
		findingsErr := errors.New(strings.TrimRight(sb.String(), "\n"))
		return restoreWorkspaceYAMLOrJoin(wsYAML, bakPath, findingsErr)
	}

	// Print kit summary from the written workspace.yaml. The backup is left
	// in place — see the doc comment above.
	meta, loadErr := store.Load(slug)
	if loadErr == nil && meta != nil {
		fmt.Fprintf(out, "kits: %s\n", formatStringSlice(meta.Kits))
	}
	return nil
}

// runWorkspaceRemove deletes a workspace via DELETE /api/workspaces/{slug}
// (docs/plans/workspace-db-consolidation.md PR4 Step H). Unlike the pre-PR4
// CLI, this no longer blocks on (or even checks for) assigned projects
// first: WorkspaceRepository.Remove's transaction (decision 8, wired in
// Step F) already re-assigns any assigned project to the default workspace
// as part of the same delete, so there is nothing left to clear by hand
// first.
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

	c := client.NewUnixClient(client.DefaultSocketPath())

	if err := c.Do("DELETE", "/api/workspaces/"+slug, nil, nil); err != nil {
		return fmt.Errorf("remove workspace: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "workspace %q removed (any assigned projects were re-assigned to %q).\n",
		slug, orchestrator.DefaultWorkspaceSlug)
	return nil
}

// runWorkspaceExport exports a workspace's definition as yaml via
// GET /api/workspaces/{slug}/export (docs/plans/workspace-db-consolidation.md
// PR5 Step D). Unlike every other workspace subcommand, this deliberately
// does not go through renderOutput: the whole point is to emit the raw yaml
// document unchanged (to stdout, or --output <path>), not a
// json/yaml/plain-text rendering of a structured response object.
func runWorkspaceExport(cmd *cobra.Command, args []string) error {
	slug := args[0]
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return err
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	statusCode, body, err := c.GetRawWithAccept("/api/workspaces/"+slug+"/export", "application/yaml")
	if err != nil {
		return fmt.Errorf("export workspace: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("export workspace: %s", formatWorkspaceAPIError(statusCode, body))
	}

	if workspaceExportOutput != "" {
		if err := os.WriteFile(workspaceExportOutput, body, 0o644); err != nil {
			return fmt.Errorf("write --output: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "workspace %q exported to %s\n", slug, workspaceExportOutput)
		return nil
	}

	_, err = cmd.OutOrStdout().Write(body)
	return err
}

// runWorkspaceImport imports a workspace definition from a yaml file via
// POST /api/workspaces/import?mode=<create-only|replace>
// (docs/plans/workspace-db-consolidation.md PR5 Step E).
//
// The export endpoint's body (GET .../export) deliberately carries no
// top-level "slug" key — the slug is already known from the URL it hangs
// off of — so import must recover a target slug from elsewhere: --slug when
// given, otherwise the import file's basename with its extension stripped
// (e.g. "team-a.yaml" -> "team-a"). This asymmetry is why `workspace
// export`'s output cannot be piped straight back into `workspace import`
// without either naming the file after the slug or passing --slug
// explicitly.
//
// --mode defaults to "create-only" (the safe choice per docs/plans/
// workspace-db-consolidation.md's PR5 note recommending it as the default
// where the plan doc itself leaves this unspecified); --force is a shorthand
// for --mode replace, mirroring `workspace edit`'s own --force convention
// even though the underlying semantics differ (edit's --force skips an
// If-Match check on an existing row; import's --mode replace is a full
// create-or-overwrite upsert).
func runWorkspaceImport(cmd *cobra.Command, args []string) error {
	file := args[0]

	// --force is a shorthand for --mode replace, but only when --mode was
	// left at its default. If the caller explicitly set --mode to a value
	// other than "replace" *and* also passed --force, the two directives
	// disagree — fail loudly rather than silently letting --force overwrite
	// an explicit "create-only" (codex PR5 review, minor: --force が
	// silent に上書きするのは危険、 特に "--mode create-only" は明示的な
	// 安全宣言なので勝手に replace に翻訳しない).
	modeFlag := cmd.Flags().Lookup("mode")
	modeExplicit := modeFlag != nil && modeFlag.Changed
	mode := workspaceImportMode
	if workspaceImportForce {
		if modeExplicit && workspaceImportMode != "replace" {
			return fmt.Errorf("--force conflicts with --mode %q; either drop --force or set --mode replace explicitly", workspaceImportMode)
		}
		mode = "replace"
	}
	switch mode {
	case "create-only", "replace":
	default:
		return fmt.Errorf("invalid --mode %q (want create-only or replace)", mode)
	}

	slug := workspaceImportSlug
	if slug == "" {
		base := filepath.Base(file)
		slug = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return fmt.Errorf("resolve target slug (pass --slug explicitly): %w", err)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}

	// Client-side fail-fast strict validation before making any daemon call
	// — the same double-validation pattern `workspace create`/`workspace
	// edit` already follow (see buildWorkspaceCreateBody's and
	// runWorkspaceEdit's doc comments): the server runs the identical check
	// again on the constructed body below, but failing here saves a round
	// trip and works even with no daemon reachable.
	if _, err := orchestrator.DecodeWorkspaceMetaStrict(data); err != nil {
		return fmt.Errorf("validate %s: %w", file, err)
	}

	body, err := buildWorkspaceCreateBody(slug, data)
	if err != nil {
		return fmt.Errorf("build import request: %w", err)
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	statusCode, respBody, err := c.PostRaw("/api/workspaces/import?mode="+mode, "application/yaml", body)
	if err != nil {
		return fmt.Errorf("import workspace: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("import workspace: %s", formatWorkspaceAPIError(statusCode, respBody))
	}

	var detail api.WorkspaceDetail
	if err := json.Unmarshal(respBody, &detail); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	return renderOutput(cmd, &detail, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace imported: %s (revision %s, mode %s)\n", detail.Slug, detail.Revision, mode)
		return nil
	})
}

// formatStringSlice formats a slice for display: "(none)" when empty, or comma-joined.
func formatStringSlice(ss []string) string {
	if len(ss) == 0 {
		return "(none)"
	}
	return strings.Join(ss, ", ")
}
