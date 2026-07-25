package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/initwizard"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// --workspace flag values for project add / project init.
var projectAddWorkspace string
var projectInitWorkspace string

// --name flag for project add: overrides the project name otherwise derived
// from the git URL's last path component (docs/plans/volume-only-daemon.md
// §論点a).
var projectAddName string

// --agent flag value for project init; empty falls back to initwizard.DefaultAgent.
var projectInitAgent string

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage projects",
}

// mustCanonicalBehavior returns the canonical name for a known alias. It is
// used by display code to dedupe back-compat alias mirror entries — callers
// should only invoke it after IsBehaviorAliasKey has returned true.
func mustCanonicalBehavior(alias string) string {
	canonical, _ := projectspec.CanonicalBehaviorName(alias)
	return canonical
}

// projectAddCmd is scopeRemote as of docs/plans/volume-only-daemon.md
// §論点a's cutover — the reclassification this command's OLD doc comment
// (codex review round 2, docs/plans/cli-remote-connection.md classification
// table) predicted: "Phase 6 ... is expected to move project registration
// to a remote-git-URL model, at which point this reclassifies to
// scopeRemote". The old form, `boid project add <dir>`, resolved a path on
// the machine the CLI process runs on against the DAEMON's own filesystem
// (reads .boid/project.yaml, captures the git origin remote) — that only
// ever worked because there was no remote-daemon transport yet, and would
// resolve against the wrong host's filesystem under a future non-loopback
// profile. The new form, `boid project add <git-url>`, has no local
// filesystem dependency at all: the daemon clones the URL itself into a
// daemon-managed bare repository (see internal/api.ProjectAppService.
// CreateProjectFromGitURL) — a plain HTTP API operation like any other
// scopeRemote command.
var projectAddCmd = &cobra.Command{
	Use:   "add <git-url>",
	Short: "Register a project from a git remote URL",
	Long: `Register a project by having the daemon clone a git remote URL into a
daemon-managed bare repository (docs/plans/volume-only-daemon.md §論点a/b).
--workspace is required; --name overrides the project name otherwise
derived from the URL's last path component.

Host directory registration (the pre-volume-only "boid project add <dir>"
form) was retired in the volume-only cutover: the daemon container has no
access to host checkouts any more, only to git remote URLs it can clone
itself. If <dir> is passed here it is rejected with this same message —
push it upstream first, then register the URL:

  boid project add <git-url> --workspace=<name> [--name=<project-name>]
`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runProjectAdd,
}

var projectInitSubCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "Initialize a new boid project interactively and register it",
	Long: `Initialize a new boid project in the current directory (or [dir]).

Prompts for a project name, then writes .boid/project.yaml with the canonical
supervisor / executor task_behaviors (agent=claude-code by default) and
registers the project with the running boid daemon. project.yaml only holds
id / name / task_behaviors / default_task_behavior — runtime environment
config (` + "`host_commands`" + ` / ` + "`env`" + ` / ` + "`additional_bindings`" + ` / ` + "`allowed_domains`" + `)
lives separately in a workspace; set it up with
` + "`boid workspace create/edit/import`" + ` (see the --workspace flag below).

Optionally assigns the project to a workspace (get-or-create: creates a DB
row for the slug even if no workspace.yaml exists yet).

Example:
  boid project init                              # initialize in current dir
  boid project init ./my-project                 # initialize in ./my-project
  boid project init . --workspace main           # also assign to workspace "main"
  boid project init . --agent codex              # bake a non-default agent
`,
	// scopeLocal — same "境界越えで壊れる" rationale as projectAddCmd above:
	// [dir] is a local filesystem path the daemon resolves against its own
	// host.
	Args:        cobra.MaximumNArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeLocal},
	RunE:        runProjectInit,
}

var projectListCmd = &cobra.Command{
	Use:         "list",
	Short:       "List registered projects",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runProjectList,
}

var projectRemoveCmd = &cobra.Command{
	Use:               "remove <project-ref>",
	Short:             "Remove a project (id or name, partial match supported)",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectRemove,
}

// projectReloadCmd is scopeLocal, grouped with add/init in the plan doc's
// "境界越えで壊れる" row even though reload itself takes no path argument —
// runProjectReload re-reads every registered project's .boid/project.yaml
// from its stored WorkDir and re-captures each one's git origin remote, both
// of which only resolve correctly against the daemon's own host filesystem.
var projectReloadCmd = &cobra.Command{
	Use:         "reload",
	Short:       "Reload project.yaml for all registered projects",
	Annotations: map[string]string{scopeAnnotationKey: scopeLocal},
	RunE:        runProjectReload,
}

var projectShowCmd = &cobra.Command{
	Use:               "show <project-ref>",
	Short:             "Show project details (id or name, partial match supported)",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectShow,
}

var projectBehaviorsCmd = &cobra.Command{
	Use:               "behaviors <project-ref>",
	Short:             "List task behaviors defined in the project (id or name, partial match supported)",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectBehaviors,
}

// projectFetchCmd is scopeRemote: it runs `git fetch --all` inside a
// git-URL registered project's daemon-managed bare repo and reloads
// project.yaml (docs/plans/volume-only-daemon.md §論点b fetch 経路) — a
// plain HTTP API operation, same as `project show`/`project list`.
var projectFetchCmd = &cobra.Command{
	Use:               "fetch <project-ref>",
	Short:             "Fetch the latest git refs for a git-URL registered project and reload project.yaml",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectFetch,
}

func init() {
	projectAddCmd.Flags().StringVar(&projectAddWorkspace, "workspace", "", "Workspace to register the project into (required, get-or-create)")
	projectAddCmd.Flags().StringVar(&projectAddName, "name", "", "Project name override (default: derived from the git URL's last path component)")
	projectInitSubCmd.Flags().StringVar(&projectInitWorkspace, "workspace", "", "Assign the project to a workspace after initialization (get-or-create)")
	projectInitSubCmd.Flags().StringVar(&projectInitAgent, "agent", "", "Harness agent baked into each behavior's default_instruction (default: claude-code)")

	projectCmd.AddCommand(projectAddCmd, projectInitSubCmd, projectListCmd, projectRemoveCmd, projectReloadCmd, projectFetchCmd, projectShowCmd, projectBehaviorsCmd)
	rootCmd.AddCommand(projectCmd)
}

// looksLikeGitURL is the "clear error" heuristic docs/plans/
// volume-only-daemon.md §論点a asks for: distinguish a git remote URL from
// a host filesystem path BEFORE the CLI ever talks to the daemon, so
// `boid project add <dir>` fails fast with actionable guidance instead of a
// confusing daemon-side clone error (or, worse, git silently treating <dir>
// as a local clone source — a directory path IS technically a valid `git
// clone` source, which would register a filesystem path in an UpstreamURL
// field meant to hold a real remote). A URL is recognized by either an
// explicit scheme (`https://`, `ssh://`, ...) or the scp-like
// `user@host:path` form (`git@github.com:owner/repo.git`) — the two shapes
// docs/plans/volume-only-daemon.md and the existing dispatcher URL parser
// (repoSlugFromOriginURL) both already treat as the complete set of
// supported git URL forms.
func looksLikeGitURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	if at := strings.Index(s, "@"); at != -1 && strings.Contains(s[at:], ":") {
		return true
	}
	return false
}

func runProjectAdd(cmd *cobra.Command, args []string) error {
	gitURL := args[0]
	if !looksLikeGitURL(gitURL) {
		return fmt.Errorf("host directory registration retired in volume-only cutover; use a git URL instead:\n  boid project add <git-url> --workspace=<name> [--name=<project-name>]\n(%q does not look like a git URL — push it upstream first if it is a local checkout)", gitURL)
	}
	if projectAddWorkspace == "" {
		return fmt.Errorf("--workspace is required: boid project add <git-url> --workspace=<name>")
	}
	if err := projectspec.ValidWorkspaceSlug(projectAddWorkspace); err != nil {
		return fmt.Errorf("invalid --workspace value: %w", err)
	}

	c := client.FromContext(cmd.Context())

	// get-or-create the workspace client-side (same UX as project init's
	// --workspace flag / `boid workspace assign`) so a typo'd-but-plausible
	// new slug does not require a separate `boid workspace create` round
	// trip. The server-side CreateProjectFromGitURL still validates the
	// workspace exists (404 otherwise) as a belt-and-suspenders check —
	// see that method's own doc comment for why it does not itself
	// get-or-create (a git-URL registration has no host-dir fallback to
	// eagerly default into like the legacy CreateProject flow does).
	if err := ensureWorkspaceExistsGetOrCreate(c, projectAddWorkspace, cmd.OutOrStdout()); err != nil {
		return fmt.Errorf("get-or-create workspace %q: %w", projectAddWorkspace, err)
	}

	body := map[string]string{"url": gitURL, "workspace": projectAddWorkspace}
	if projectAddName != "" {
		body["name"] = projectAddName
	}

	var p projectspec.Project
	if err := c.Do("POST", "/api/projects/git", body, &p); err != nil {
		return fmt.Errorf("register project: %w", err)
	}

	return renderOutput(cmd, &p, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "project registered: %s (%s)\n", p.ID, p.Meta.Name)
		fmt.Fprintf(cmd.OutOrStdout(), "  workspace: %s\n", p.WorkspaceID)
		fmt.Fprintf(cmd.OutOrStdout(), "  bare repo: %s\n", p.WorkDir)
		// Check hook requires
		for _, b := range p.Meta.TaskBehaviors {
			for _, h := range b.Hooks {
				for _, req := range h.Requires {
					if _, err := exec.LookPath(req); err != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "  warning: hook %q requires %q but it's not found in PATH\n", h.ID, req)
					}
				}
			}
		}
		return nil
	})
}

// runProjectFetch handles `boid project fetch <project-ref>` (docs/plans/
// volume-only-daemon.md §論点b fetch 経路).
func runProjectFetch(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	var updated projectspec.Project
	if err := c.Do("POST", "/api/projects/"+p.ID+"/fetch", nil, &updated); err != nil {
		return fmt.Errorf("fetch project: %w", err)
	}

	return renderOutput(cmd, &updated, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "project fetched: %s (%s)\n", updated.ID, updated.Meta.Name)
		return nil
	})
}

// runProjectInit runs the interactive init wizard then registers and (optionally) assigns workspace.
func runProjectInit(cmd *cobra.Command, args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}

	projectDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	// Abort if project.yaml already exists.
	projectYAMLPath := filepath.Join(projectDir, ".boid", "project.yaml")
	if _, err := os.Stat(projectYAMLPath); err == nil {
		return fmt.Errorf(".boid/project.yaml already exists in %s; remove it first", projectDir)
	}

	w := &initwizard.Wizard{
		In:    os.Stdin,
		Out:   cmd.OutOrStdout(),
		Agent: projectInitAgent,
	}

	if err := w.Run(projectDir); err != nil {
		return err
	}

	// Register with daemon.
	c := client.FromContext(cmd.Context())
	var p projectspec.Project
	if err := c.Do("POST", "/api/projects", map[string]string{"work_dir": projectDir}, &p); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not register project with boid server: %v\n", err)
		fmt.Fprintln(os.Stderr, "Run 'boid project add .' once the server is running.")
		return nil
	}

	// Optionally assign workspace (get-or-create).
	if projectInitWorkspace != "" {
		if err := assignProjectWorkspace(c, p.ID, projectInitWorkspace, cmd.OutOrStdout()); err != nil {
			return err
		}
		p.WorkspaceID = projectInitWorkspace
	}

	fmt.Fprintf(cmd.OutOrStdout(), "project registered: %s (%s)\n", p.ID, p.Meta.Name)
	if p.WorkspaceID != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "  workspace: %s\n", p.WorkspaceID)
	}
	return nil
}

// assignProjectWorkspace sends PUT /api/projects/<id>/workspace to link the
// project to a workspace. get-or-create semantics: an empty workspace is
// created for workspaceSlug first if it has no DB row yet
// (ensureWorkspaceExistsGetOrCreate, MAJOR 4 codex review,
// docs/plans/workspace-db-consolidation.md) — before that fix, this
// function only ever called the assign PUT, so an unknown slug 404'd there
// even though `project add`/`project init` had already registered the
// project (a partial-success state: project registered, workspace
// assignment failed).
//
// CLI entry-point validation per plan (3-layer defense): a non-empty slug
// must satisfy ValidWorkspaceSlug. Empty string means "clear" and is allowed
// to bypass validation (handled at the domain layer).
func assignProjectWorkspace(c *client.Client, projectID, workspaceSlug string, out io.Writer) error {
	if workspaceSlug != "" {
		if err := projectspec.ValidWorkspaceSlug(workspaceSlug); err != nil {
			return fmt.Errorf("invalid --workspace value: %w", err)
		}
		if err := ensureWorkspaceExistsGetOrCreate(c, workspaceSlug, out); err != nil {
			return fmt.Errorf("get-or-create workspace %q: %w", workspaceSlug, err)
		}
	}
	var result projectspec.Project
	if err := c.Do("PUT", "/api/projects/"+projectID+"/workspace", map[string]string{"workspace_id": workspaceSlug}, &result); err != nil {
		return fmt.Errorf("assign workspace %q: %w", workspaceSlug, err)
	}
	return nil
}

func runProjectList(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	var projects []projectspec.Project
	if err := c.Do("GET", "/api/projects", nil, &projects); err != nil {
		return err
	}

	return renderOutput(cmd, projects, func() error {
		if len(projects) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no projects registered")
			return nil
		}
		for _, p := range projects {
			status := p.Status
			if status == "" {
				status = "ready"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-9s %s  (%s)", p.ID, status, p.Meta.Name, p.WorkDir)
			if p.UpstreamURL != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  upstream=%s", p.UpstreamURL)
			}
			fmt.Fprintln(cmd.OutOrStdout())
			if p.Status != "" && p.StatusMessage != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", p.StatusMessage)
			}
		}
		return nil
	})
}

func runProjectRemove(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	var result map[string]string
	if err := c.Do("DELETE", "/api/projects/"+p.ID, nil, &result); err != nil {
		return fmt.Errorf("remove project: %w", err)
	}

	return renderOutput(cmd, map[string]any{"id": p.ID, "removed": true}, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "project removed: %s\n", p.ID)
		return nil
	})
}

func runProjectReload(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	var result map[string]any
	if err := c.Do("POST", "/api/projects/reload", nil, &result); err != nil {
		return fmt.Errorf("reload: %w", err)
	}

	return renderOutput(cmd, result, func() error {
		status := result["status"]
		fmt.Fprintf(cmd.OutOrStdout(), "reload: %s\n", status)
		if errs, ok := result["errors"]; ok {
			for _, e := range errs.([]any) {
				fmt.Fprintf(cmd.OutOrStdout(), "  error: %s\n", e)
			}
		}
		return nil
	})
}

func runProjectShow(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	return renderOutput(cmd, p, func() error {
		renderProjectDetail(p)
		return nil
	})
}

func runProjectBehaviors(cmd *cobra.Command, args []string) error {
	c := client.FromContext(cmd.Context())

	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	return renderOutput(cmd, p.Meta.TaskBehaviors, func() error {
		renderProjectBehaviors(p)
		return nil
	})
}

func renderProjectDetail(p *projectspec.Project) {
	fmt.Printf("ID:          %s\n", p.ID)
	fmt.Printf("Name:        %s\n", p.Meta.Name)
	fmt.Printf("WorkDir:     %s\n", p.WorkDir)
	fmt.Printf("WorkspaceID: %s\n", p.WorkspaceID)
	if p.UpstreamURL != "" {
		fmt.Printf("UpstreamURL: %s\n", p.UpstreamURL)
	} else {
		fmt.Printf("UpstreamURL: (none — add a git remote and run `boid project reload`)\n")
	}
	fmt.Printf("CreatedAt:   %s\n", formatTime(p.CreatedAt))
	fmt.Printf("UpdatedAt:   %s\n", formatTime(p.UpdatedAt))
	if p.Status != "" {
		// Status is omitted from the API response (and thus zero here) for
		// the common "ready" case — see orchestrator.Project.Status's doc
		// comment — so only print the line when there is something to say.
		fmt.Printf("Status:      %s\n", p.Status)
		if p.StatusMessage != "" {
			fmt.Printf("             %s\n", p.StatusMessage)
		}
	}

	m := p.Meta

	if len(m.TaskBehaviors) > 0 {
		fmt.Println("TaskBehaviors:")
		keys := make([]string, 0, len(m.TaskBehaviors))
		for k := range m.TaskBehaviors {
			// Skip back-compat alias mirror entries: a behavior named
			// "plan" with a canonical "supervisor" entry would otherwise
			// be listed twice. The canonical entry is the one of record.
			if projectspec.IsBehaviorAliasKey(k) {
				if _, hasCanonical := m.TaskBehaviors[mustCanonicalBehavior(k)]; hasCanonical {
					continue
				}
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b := m.TaskBehaviors[k]
			fmt.Printf("  %-20s\n", k)
			for _, h := range b.Hooks {
				requires := ""
				if len(h.Requires) > 0 {
					requires = "  requires=[" + strings.Join(h.Requires, ",") + "]"
				}
				fmt.Printf("    hook: %s%s\n", h.ID, requires)
			}
		}
	}

	if len(m.HostCommands) > 0 {
		fmt.Println("HostCommands:")
		hcKeys := make([]string, 0, len(m.HostCommands))
		for k := range m.HostCommands {
			hcKeys = append(hcKeys, k)
		}
		sort.Strings(hcKeys)
		for _, k := range hcKeys {
			fmt.Printf("  %s\n", k)
		}
	}

	if len(m.AdditionalBindings) > 0 {
		fmt.Println("AdditionalBindings:")
		for _, b := range m.AdditionalBindings {
			fmt.Printf("  %s  (%s)\n", b.Source, b.Mode)
		}
	}

	if len(m.Env) > 0 {
		fmt.Println("Env:")
		envKeys := make([]string, 0, len(m.Env))
		for k := range m.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			fmt.Printf("  %s\n", k)
		}
	}

	if m.SecretNamespace != "" {
		fmt.Printf("SecretNamespace: %s\n", m.SecretNamespace)
	}
}

func renderProjectBehaviors(p *projectspec.Project) {
	if len(p.Meta.TaskBehaviors) == 0 {
		fmt.Println("no behaviors defined")
		return
	}

	keys := make([]string, 0, len(p.Meta.TaskBehaviors))
	for k := range p.Meta.TaskBehaviors {
		if projectspec.IsBehaviorAliasKey(k) {
			if _, hasCanonical := p.Meta.TaskBehaviors[mustCanonicalBehavior(k)]; hasCanonical {
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		b := p.Meta.TaskBehaviors[k]
		fmt.Printf("%-20s\n", k)
		if len(b.Traits) > 0 {
			fmt.Printf("  traits: %s\n", strings.Join(b.Traits, ", "))
		}
	}
	// Project-level base_branch (Phase 3-1: behavior-level readonly / worktree /
	// branch_prefix / base_branch are gone; branch-policy-simplification Phase 2
	// additionally retired the project-top 'worktree' field, so only
	// base_branch remains to display).
	if p.Meta.BaseBranch != "" {
		fmt.Printf("\nbase_branch: %s\n", p.Meta.BaseBranch)
	}
}
