package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// --workspace flag values for project add / project init.
var projectAddWorkspace string

// --name flag for project add: overrides the project name otherwise derived
// from the git URL's last path component.
var projectAddName string
var projectAddPerm string

// --agent flag value for project init; empty falls back to initwizard.DefaultAgent.

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage projects",
}

var projectAddCmd = &cobra.Command{
	Use:   "add <git-url>",
	Short: "Register a project from a git remote URL",
	Long: `Register a project from a git remote URL:

  boid project add <git-url> --workspace=<name> [--name=<project-name>] [--perm=fetch|push]

Has the daemon clone the URL itself into a daemon-managed bare repository
(docs/plans/volume-only-daemon.md §論点a/b). --workspace is required;
--name overrides the project name otherwise derived from the URL's last
path component.

A git URL is recognized by an explicit scheme (https://, ssh://, git://,
...) or the scp-like form (user@host:path, e.g.
git@github.com:owner/repo.git). Anything else (including a bare relative or
absolute filesystem path) is rejected — host-directory registration was
removed; migrate an existing one with 'boid project rm <ref>' followed by
this command.
`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runProjectAdd,
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

var projectReloadCmd = &cobra.Command{
	Use:         "reload",
	Short:       "Refresh registered repository identities",
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runProjectReload,
}

var projectShowExplain bool

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

var projectFetchCmd = &cobra.Command{
	Use:               "fetch <project-ref>",
	Short:             "Fetch the latest git refs for a git-URL registered project and refresh the repository identity",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeProjectRefs,
	Annotations:       map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:              runProjectFetch,
}

var projectSetPermCmd = &cobra.Command{
	Use:         "set-perm <project-ref> <fetch|push>",
	Short:       "Set a registered repository's permission",
	Args:        cobra.ExactArgs(2),
	Annotations: map[string]string{scopeAnnotationKey: scopeRemote},
	RunE:        runProjectSetPerm,
}

func runProjectSetPerm(cmd *cobra.Command, args []string) error {
	if err := projectspec.ValidateProjectPerm(args[1]); err != nil {
		return err
	}
	c := client.FromContext(cmd.Context())
	p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	var updated projectspec.Project
	if err := c.Do("PUT", "/api/projects/"+p.ID+"/perm", map[string]string{"perm": args[1]}, &updated); err != nil {
		return err
	}
	return renderOutput(cmd, &updated, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "project permission updated: %s perm=%s\n", updated.ID, updated.Perm)
		return nil
	})
}

func init() {
	projectAddCmd.Flags().StringVar(&projectAddPerm, "perm", "push", "Repository permission: fetch or push")
	projectAddCmd.Flags().StringVar(&projectAddWorkspace, "workspace", "", "Workspace to register the project into (required for a git-URL <git-url>; optional, get-or-create, for a legacy <dir>)")
	projectAddCmd.Flags().StringVar(&projectAddName, "name", "", "Project name override, git-URL form only (default: derived from the URL's last path component)")
	projectShowCmd.Flags().BoolVar(&projectShowExplain, "explain", false, "Show field-by-field provenance (workspace default vs unset)")

	projectRemoveCmd.Aliases = []string{"rm"}

	projectCmd.AddCommand(projectSetPermCmd, projectAddCmd, projectListCmd, projectRemoveCmd, projectReloadCmd, projectFetchCmd, projectShowCmd, projectBehaviorsCmd)
	rootCmd.AddCommand(projectCmd)
}

var gitURLSchemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

var scpLikeGitURLPattern = regexp.MustCompile(`^([a-zA-Z0-9_.-]+@)?[a-zA-Z0-9_.-]+:[^/]`)

func looksLikeGitURL(s string) bool {
	if gitURLSchemePattern.MatchString(s) {
		return true
	}
	return scpLikeGitURLPattern.MatchString(s)
}

func runProjectAdd(cmd *cobra.Command, args []string) error {
	arg := args[0]
	if !looksLikeGitURL(arg) {
		return fmt.Errorf("host directory registration was removed; use `boid project add <git-url> --workspace=<name>`")
	}
	return runProjectAddGitURL(cmd, arg)
}

func runProjectAddGitURL(cmd *cobra.Command, gitURL string) error {
	perm := projectAddPerm
	if perm == "" {
		perm = "push"
	}
	if err := projectspec.ValidateProjectPerm(perm); err != nil {
		return err
	}
	if projectAddWorkspace == "" {
		return fmt.Errorf("--workspace is required: boid project add <git-url> --workspace=<name>")
	}
	if err := projectspec.ValidWorkspaceSlug(projectAddWorkspace); err != nil {
		return fmt.Errorf("invalid --workspace value: %w", err)
	}

	c := client.FromContext(cmd.Context())

	if err := ensureWorkspaceExistsGetOrCreate(c, projectAddWorkspace, cmd.OutOrStdout()); err != nil {
		return fmt.Errorf("get-or-create workspace %q: %w", projectAddWorkspace, err)
	}

	body := map[string]string{"url": gitURL, "workspace": projectAddWorkspace, "perm": perm}
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
		fmt.Fprintf(cmd.OutOrStdout(), "  perm: %s\n", p.Perm)
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

// runProjectFetch handles `boid project fetch <project-ref>`.
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

func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-9s %s  (%s)  perm=%s", p.ID, status, p.Meta.Name, p.WorkDir, p.Perm)
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

	var explain *projectspec.ProjectExplain
	if e, eerr := fetchProjectExplain(c, p.ID); eerr == nil {
		explain = e
	} else if projectShowExplain {
		return fmt.Errorf("get project explain: %w", eerr)
	}

	out := &projectShowOutput{Project: p}
	if explain != nil {
		out.WorkspaceDefaultApplied = explain.AnyWorkspaceDefaultApplied
		if projectShowExplain {
			out.Explain = explain
		}
	}

	return renderOutput(cmd, out, func() error {
		renderProjectDetail(p)
		if explain != nil {
			renderProjectExplainSummary(explain)
			if projectShowExplain {
				renderProjectExplainDetail(explain)
			}
		}
		return nil
	})
}

type projectShowOutput struct {
	*projectspec.Project `yaml:",inline"`

	WorkspaceDefaultApplied bool `json:"workspace_default_applied,omitempty" yaml:"workspace_default_applied,omitempty"`

	Explain *projectspec.ProjectExplain `json:"explain,omitempty" yaml:"explain,omitempty"`
}

func fetchProjectExplain(c *client.Client, projectID string) (*projectspec.ProjectExplain, error) {
	var explain projectspec.ProjectExplain
	if err := c.Do("GET", "/api/projects/"+projectID+"/explain", nil, &explain); err != nil {
		return nil, err
	}
	return &explain, nil
}

func renderProjectExplainSummary(e *projectspec.ProjectExplain) {
	if e.AnyWorkspaceDefaultApplied {
		fmt.Println("Note:        one or more fields are inherited from the workspace default (see `project show --explain`)")
	}
	if e.WorkspaceUnavailable {
		fmt.Println("Note:        the linked workspace's default project definition could not be read (see `project show --explain`)")
	}
	if e.IDSource != "" {
		fmt.Println("Note:        repository id and name are resolved from the registry")
		nameSource := e.NameSource
		if nameSource == "" {
			nameSource = "(could not be recovered)"
		}
		fmt.Printf("Note:        its name source is %q (see `project show --explain` for detail)\n", nameSource)
	}
}

func renderProjectExplainDetail(e *projectspec.ProjectExplain) {
	fmt.Println("\nExplain:")
	if e.WorkspaceUnavailable {
		fmt.Println("  WARNING: the linked workspace's default project definition could not be read; any field below reading \"workspace unavailable\" means this function genuinely does not know whether a workspace default would apply — not that none does.")
	}
	fmt.Printf("  base_branch:           %s\n", e.BaseBranch)
	fmt.Printf("  fork_point:            %s\n", e.ForkPoint)
	fmt.Printf("  default_task_behavior: %s\n", e.DefaultTaskBehavior)
	if len(e.TaskBehaviors) > 0 {
		fmt.Println("  task_behaviors:")
		for _, name := range e.TaskBehaviorNames() {
			fmt.Printf("    %-20s %s\n", name, e.TaskBehaviors[name])
		}
	}
	if e.IDSource != "" {
		nameSource := e.NameSource
		if nameSource == "" {
			nameSource = "(could not be recovered)"
		}
		fmt.Printf("  id source:             %s\n", e.IDSource)
		fmt.Printf("  name source:           %s\n", nameSource)
	}
	fmt.Printf("\n  %s\n", e.BaseBranchSnapshotNote)
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
	fmt.Printf("Perm:        %s\n", p.Perm)
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
	if p.Meta.BaseBranch != "" {
		fmt.Printf("\nbase_branch: %s\n", p.Meta.BaseBranch)
	}
}
