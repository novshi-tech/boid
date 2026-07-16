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

// projectAddCmd is scopeLocal (codex review round 2, docs/plans/
// cli-remote-connection.md classification table: "境界越えで壊れる | project
// add / init / reload | work_dir のパス文字列を daemon 側 FS で解決"). This
// command's own work does go through the daemon's HTTP API today (POST
// /api/projects), which is why it used to be scopeRemote — but <dir> is a
// path on the machine the CLI process runs on, and the daemon resolves it
// against its *own* local filesystem (reads .boid/project.yaml, captures
// the git origin remote, etc.). That only ever coincides with the CLI's
// filesystem because there is no remote-daemon transport yet; against a
// future https:// profile (Phase 3), the same dir string would resolve
// against the wrong host's filesystem entirely. Phase 6
// (docs/plans/container-based-boid.md) is expected to move project
// registration to a remote-git-URL model, at which point this reclassifies
// to scopeRemote.
var projectAddCmd = &cobra.Command{
	Use:         "add <dir>",
	Short:       "Register a project from .boid/project.yaml",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeLocal},
	RunE:        runProjectAdd,
}

var projectInitSubCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "Initialize a new boid project interactively and register it",
	Long: `Initialize a new boid project in the current directory (or [dir]).

Prompts for a project name, then writes .boid/project.yaml with the canonical
supervisor / executor task_behaviors (worktree=true, agent=claude-code by
default) and registers the project with the running boid daemon. Kit
selection has moved to ` + "`boid workspace configure`" + `.

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

func init() {
	projectAddCmd.Flags().StringVar(&projectAddWorkspace, "workspace", "", "Assign the project to a workspace after registration (get-or-create)")
	projectInitSubCmd.Flags().StringVar(&projectInitWorkspace, "workspace", "", "Assign the project to a workspace after initialization (get-or-create)")
	projectInitSubCmd.Flags().StringVar(&projectInitAgent, "agent", "", "Harness agent baked into each behavior's default_instruction (default: claude-code)")

	projectCmd.AddCommand(projectAddCmd, projectInitSubCmd, projectListCmd, projectRemoveCmd, projectReloadCmd, projectShowCmd, projectBehaviorsCmd)
	rootCmd.AddCommand(projectCmd)
}

func runProjectAdd(cmd *cobra.Command, args []string) error {
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}

	c := client.NewUnixClient(client.DefaultSocketPath())

	var p projectspec.Project
	if err := c.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &p); err != nil {
		return fmt.Errorf("register project: %w", err)
	}

	// Optionally assign workspace (get-or-create: DB row is created even for unknown slug).
	if projectAddWorkspace != "" {
		if err := assignProjectWorkspace(c, p.ID, projectAddWorkspace, cmd.OutOrStdout()); err != nil {
			return err
		}
		p.WorkspaceID = projectAddWorkspace
	}

	return renderOutput(cmd, &p, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "project registered: %s (%s)\n", p.ID, p.Meta.Name)
		if p.WorkspaceID != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  workspace: %s\n", p.WorkspaceID)
		}
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
	c := client.NewUnixClient(client.DefaultSocketPath())
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
	c := client.NewUnixClient(client.DefaultSocketPath())

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
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s  (%s)", p.ID, p.Meta.Name, p.WorkDir)
			if p.UpstreamURL != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  upstream=%s", p.UpstreamURL)
			}
			fmt.Fprintln(cmd.OutOrStdout())
		}
		return nil
	})
}

func runProjectRemove(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

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
	c := client.NewUnixClient(client.DefaultSocketPath())

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
	c := client.NewUnixClient(client.DefaultSocketPath())

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
	c := client.NewUnixClient(client.DefaultSocketPath())

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
