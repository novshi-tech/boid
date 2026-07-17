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

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
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

var workspaceRemoveCmd = &cobra.Command{
	Use:   "remove <slug>",
	Short: "Remove a workspace and its home directory (DELETE /api/workspaces/{slug}; prompts for confirmation if the home dir exists; --force/--yes skips the prompt)",
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
	// Every workspace subcommand talks to the daemon's HTTP API — all
	// scopeRemote (docs/plans/workspace-db-consolidation.md decision 18).
	for _, c := range []*cobra.Command{
		workspaceListCmd, workspaceShowCmd, workspaceCreateCmd, workspaceEditCmd,
		workspaceAssignCmd, workspaceClearCmd, workspaceRemoveCmd,
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
	workspaceRemoveCmd.Flags().BoolVar(&workspaceRemoveForce, "force", false, "skip the home directory deletion confirmation prompt")
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

// runWorkspaceList lists every workspace via GET /api/workspaces
// (docs/plans/workspace-db-consolidation.md PR4 Step H): the workspaces
// table is now the single source of truth, so unlike the pre-PR4 "yaml dir
// scan + DB scan union" this needs no local filesystem read at all — an
// empty workspace (no assigned projects) is already included in the API
// response (Step B's ListWorkspaces rewrite).
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
	// Home reports the workspace home directory's on-disk size
	// (docs/plans/home-workspace-volume.md Phase 4 PR5), mirrored straight
	// from the GET /api/workspaces/{slug} response.
	Home     *api.WorkspaceHomeSize  `json:"home,omitempty"`
	Projects []*orchestrator.Project `json:"projects"`
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

	c := client.FromContext(cmd.Context())

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

	c := client.FromContext(cmd.Context())
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

	c := client.FromContext(cmd.Context())

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
// written by hand, by an e2e scenario, or historically by the now-retired
// `boid workspace configure` command — see docs/plans/
// workspace-db-consolidation.md PR6), in which case
// ensureWorkspaceExistsForAssign auto-creates the DB row from it first so
// the existing "drop a yaml, then assign" flow keeps working end to end.
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
// GET /api/config/kits-dir (MAJOR 1, codex review round 1, docs/plans/
// workspace-db-consolidation.md Phase 2.5 PR7).
//
// MAJOR 2 (codex review round 2): every failure mode — a 404 (the daemon
// does not expose this endpoint at all, e.g. a pre-PR7 binary, or a PR7+
// daemon this CLI simply failed to reach it on), a 5xx, a transport
// failure, a response body that does not decode as the expected shape, or a
// body that decodes cleanly but reports an empty kits_dir — now returns a
// hard error instead of silently falling back to this CLI process's own
// defaultKitsDir() computation. The original fallback conflated "the daemon
// told me it has none" with "I could not find out what the daemon has" —
// every case above is really the latter, and silently substituting this
// CLI's own default in any of them risks resolving (and then permanently
// persisting, via MaterializeWorkspaceKitsForPersist) a workspace's kit
// references against the wrong directory whenever a same-named kit happens
// to exist under both locations — the exact class of bug MAJOR 1 above
// introduced this endpoint to prevent in the first place. A daemon started
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
// ensureWorkspaceExistsForAssign's TOCTOU-avoidance invariant (MAJOR 3, codex
// review round 2, docs/plans/workspace-db-consolidation.md Phase 2.5 PR7) —
// that it reads workspaceDir/slug.yaml exactly once — by counting calls.
// Mirrors workspace_migration.go's readWorkspaceYAMLSnapshot /
// workspaceYAMLReadFile var (its MAJOR 5 fix, same repository, server side)
// for the identical reason.
var localWorkspaceYAMLReadFile = os.ReadFile

// ensureWorkspaceExistsForAssign implements `boid workspace assign`'s
// auto-create pre-check (docs/plans/workspace-db-consolidation.md PR4 Step
// H): if slug already has a workspaces row, this is a no-op. Otherwise, if
// a legacy local workspace.yaml exists for slug (dropped by hand, by an e2e
// scenario, or historically by the now-retired `boid workspace configure`
// command — which only ever wrote the local yaml, never a DB row — see PR6),
// its content is POSTed to the daemon so the
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
//
// MAJOR 3 (codex review round 2): workspaceDir/slug.yaml is now read exactly
// ONCE, mirroring readWorkspaceYAMLSnapshot's (workspace_migration.go) MAJOR
// 5 fix on the server side. Before this fix, this function called
// orchestrator.NewWorkspaceStore("").Load(slug) — its own independent
// os.ReadFile + loose yaml.Unmarshal — and then, further down, a SECOND,
// completely independent os.ReadFile of the very same path to extract a
// legacy `kits:` list and strictly validate the remainder. That second
// read's failure was silently swallowed by an `if raw, readErr := ...;
// readErr == nil { ... }` guard: on any failure there (or even a failure to
// resolve the workspace directory at all) meta stayed the first, loose-
// parsed, kits-oblivious read, and this function fell straight through to
// marshaling and POSTing THAT — silently skipping kit resolution instead of
// surfacing the second read's error, exactly the "hard-error masking" bug
// class MINOR 3-b above already fixed once for the *first* read. Worse, an
// atomic rename landing between the two reads could hand this function a
// "meta from the old file version + kits from the new file version" hybrid
// that never existed on disk at any single instant — the same TOCTOU class
// PR7's own server-side migration code flagged and fixed once before (MAJOR
// 5, readWorkspaceYAMLSnapshot). Reading the raw bytes once and deriving
// both kitRefs (extractLegacyWorkspaceKitRefs) and meta
// (DecodeWorkspaceMetaStrict, which conveniently already both validates AND
// decodes — no separate loose parse is needed at all) from that single
// snapshot makes both failure modes impossible.
func ensureWorkspaceExistsForAssign(c *client.Client, slug string, out io.Writer) error {
	if err := c.Do("GET", "/api/workspaces/"+slug, nil, &api.WorkspaceDetail{}); err == nil {
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

	// Phase 2.5 PR7 (docs/plans/workspace-db-consolidation.md, decision 12):
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

	// MINOR 1 (codex review round 3, docs/plans/workspace-db-consolidation.md):
	// DecodeWorkspaceMetaStrict both validates (typo'd/unknown fields,
	// multi-document bodies) AND decodes rest into the WorkspaceMeta used
	// below — replacing the old separate "loose parse via store.Load, then
	// re-validate the same content strictly just for its error" two-step.
	meta, err := orchestrator.DecodeWorkspaceMetaStrict(rest)
	if err != nil {
		return fmt.Errorf("validate local workspace.yaml %q for auto-create: %w", slug, err)
	}

	// MAJOR 2 (codex review round 1, docs/plans/workspace-db-consolidation.md):
	// an unresolvable kit ref now aborts the auto-create instead of
	// being swallowed as a "note:" and silently creating the
	// workspace without the kit's host_commands/env/bindings. The
	// prior best-effort convention (matching
	// postWorkspaceCreateBestEffort's own tolerance for the *create
	// call itself* racing/failing) was too permissive here: unlike a
	// concurrent-create 409 — where a workspace already exists with
	// presumably-correct content — a kit resolution failure means
	// the workspace this function is about to POST would silently
	// omit content the on-disk `kits:` reference explicitly asked
	// for, and the DB row created from it can never be
	// re-materialized afterward (MaterializeWorkspaceKitsForPersist
	// is a client-side, create-time-only expansion; nothing re-runs
	// it once the row exists). This is also the fix for a real
	// regression from PR6: PR6's daemon-side CreateWorkspace 400'd
	// on an unresolved kit reference, so `boid workspace assign`
	// itself exited non-zero; PR7's client-side materialize step had
	// silently downgraded that into a warning, letting `assign`
	// report success for a workspace it just built incompletely.
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
// counterpart, workspaceMetaStrict) no longer has a Kits field at all (Phase
// 2.5 PR7, docs/plans/workspace-db-consolidation.md decision 12) —
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
// MAJOR 3 (codex review round 1, docs/plans/workspace-db-consolidation.md):
// a second "---"-delimited document in raw is rejected up front, before any
// unmarshal-to-map-then-remarshal happens below. The previous implementation
// called a plain yaml.Unmarshal(raw, &doc) straight into a map[string]any —
// which, like every single-document yaml.Unmarshal call, silently reads only
// the first document and discards the rest — and then, whenever a `kits:`
// key was present, remarshaled that already-truncated map as `rest`. Because
// `rest` was a *fresh* marshal of the truncated first-document map (not raw
// itself), the caller's later orchestrator.DecodeWorkspaceMetaStrict(rest)
// call could no longer see the dropped second document at all, defeating
// PR4/PR5's multi-document reject exactly in the one case (`kits:` present)
// this function exists to handle. Deciding trailing-document-ness from raw
// directly, before doc/rest ever exist, closes that hole.
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
	// yaml.v3 map decoding — previously accepted by the loose per-workspace
	// yaml path pre-PR7 as "no kit refs, but the key is present"; treat them
	// the same as the key being absent here (codex PR7 review, round 3:
	// hardening extractLegacyWorkspaceKitRefs must not regress against the
	// null/empty case that existing shadow yaml files or hand-typed configs
	// still legitimately carry). Splitting the assertion also strips the
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
// `workspace remove`: skips the home-directory deletion confirmation prompt
// (docs/plans/home-workspace-volume.md Phase 4 PR5).
var workspaceRemoveForce bool

// workspaceRemoveConfirmPrompt reads the y/N answer to `workspace remove`'s
// home-directory deletion confirmation. A package-level var (rather than a
// direct call) so tests can stub it without a real TTY — mirrors
// cmd/start_automigrate.go's autoMigratePrompter injection for the same
// reason.
var workspaceRemoveConfirmPrompt = defaultWorkspaceRemoveConfirmPrompt

// defaultWorkspaceRemoveConfirmPrompt reads a y/N answer from in. Anything
// other than "y"/"yes" (case-insensitive) is treated as decline, mirroring
// cmd/start_automigrate.go's defaultMigratePrompter convention.
func defaultWorkspaceRemoveConfirmPrompt(in io.Reader) (bool, error) {
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return false, nil
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes", nil
}

// runWorkspaceRemove deletes a workspace via DELETE /api/workspaces/{slug}
// (docs/plans/workspace-db-consolidation.md PR4 Step H). Unlike the pre-PR4
// CLI, this no longer blocks on (or even checks for) assigned projects
// first: WorkspaceRepository.Remove's transaction (decision 8, wired in
// Step F) already re-assigns any assigned project to the default workspace
// as part of the same delete, so there is nothing left to clear by hand
// first.
//
// docs/plans/home-workspace-volume.md Phase 4 PR5 adds a 2-step flow on top
// of that: unless --force is given, the workspace's current home directory
// size is fetched first (GET /api/workspaces/{slug}, the same `workspace
// show` endpoint — no separate dry-run endpoint needed) and, only when a
// home directory actually exists on disk, a y/N confirmation is required
// before the DELETE call is made at all. A workspace whose home dir was
// never created (never dispatched into) skips the prompt entirely — nothing
// destructive is about to happen on disk either way.
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
		var detail api.WorkspaceDetail
		if err := c.Do("GET", "/api/workspaces/"+slug, nil, &detail); err != nil {
			return fmt.Errorf("fetch workspace before remove: %w", err)
		}
		if detail.Home != nil && detail.Home.Exists {
			fmt.Fprintln(out, formatWorkspaceHomeSize(detail.Home))
			fmt.Fprintf(out, "workspace %q の home dir を削除しますか? [y/N]: ", slug)
			proceed, err := workspaceRemoveConfirmPrompt(os.Stdin)
			if err != nil {
				return fmt.Errorf("confirm prompt: %w", err)
			}
			if !proceed {
				fmt.Fprintf(out, "aborted: workspace %q was not removed\n", slug)
				return nil
			}
		}
	}

	var resp api.WorkspaceRemoveResponse
	if err := c.Do("DELETE", "/api/workspaces/"+slug, nil, &resp); err != nil {
		return fmt.Errorf("remove workspace: %w", err)
	}

	fmt.Fprintf(out, "workspace %q removed (any assigned projects were re-assigned to %q).\n",
		slug, orchestrator.DefaultWorkspaceSlug)
	switch {
	case resp.HomeDeleted:
		fmt.Fprintf(out, "home dir deleted: %s (%s)\n", resp.HomePath, humanize.FormatBytes(resp.HomeBytes))
	case resp.HomeDeleteError != "":
		fmt.Fprintf(out, "warning: home dir delete failed (%s): %s\n", resp.HomePath, resp.HomeDeleteError)
	}
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

	c := client.FromContext(cmd.Context())
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

	c := client.FromContext(cmd.Context())
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

// formatWorkspaceHomeSize renders a *api.WorkspaceHomeSize as a single
// human-readable line for `boid workspace show` (docs/plans/
// home-workspace-volume.md Phase 4 PR5's three display cases): a normal
// size + path, "0 B (未作成: <path>)" for a workspace never dispatched
// into, or "?" when the daemon could not compute the size (e.g. permission
// denied) — matching the "never fail the whole command over a size lookup"
// contract the plan doc sets for this feature.
func formatWorkspaceHomeSize(h *api.WorkspaceHomeSize) string {
	switch {
	case h.SizeError != "":
		return fmt.Sprintf("home size: ? (%s)", h.Path)
	case !h.Exists:
		return fmt.Sprintf("home size: 0 B (未作成: %s)", h.Path)
	default:
		return fmt.Sprintf("home size: %s (%s)", humanize.FormatBytes(h.Bytes), h.Path)
	}
}
