package cmd

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/client"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage projects",
}

var projectAddCmd = &cobra.Command{
	Use:   "add <dir>",
	Short: "Register a project from .boid/project.yaml",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectAdd,
}

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered projects",
	RunE:  runProjectList,
}

var projectRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a project",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectRemove,
}

var projectReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Reload project.yaml for all registered projects",
	RunE:  runProjectReload,
}

var projectShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show project details",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectShow,
}

var projectBehaviorsCmd = &cobra.Command{
	Use:   "behaviors <id>",
	Short: "List task behaviors defined in the project",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectBehaviors,
}

func init() {
	projectCmd.AddCommand(projectAddCmd, projectListCmd, projectRemoveCmd, projectReloadCmd, projectShowCmd, projectBehaviorsCmd)
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

	fmt.Printf("project registered: %s (%s)\n", p.ID, p.Meta.Name)

	// Check hook requires
	for _, h := range p.Meta.Hooks {
		for _, req := range h.Requires {
			if _, err := exec.LookPath(req); err != nil {
				fmt.Printf("  warning: hook %q requires %q but it's not found in PATH\n", h.ID, req)
			}
		}
	}

	return nil
}

func runProjectList(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var projects []projectspec.Project
	if err := c.Do("GET", "/api/projects", nil, &projects); err != nil {
		return err
	}

	if len(projects) == 0 {
		fmt.Println("no projects registered")
		return nil
	}

	for _, p := range projects {
		fmt.Printf("%-20s %s  (%s)\n", p.ID, p.Meta.Name, p.WorkDir)
	}
	return nil
}

func runProjectRemove(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var result map[string]string
	if err := c.Do("DELETE", "/api/projects/"+args[0], nil, &result); err != nil {
		return fmt.Errorf("remove project: %w", err)
	}

	fmt.Printf("project removed: %s\n", args[0])
	return nil
}

func runProjectReload(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var result map[string]any
	if err := c.Do("POST", "/api/projects/reload", nil, &result); err != nil {
		return fmt.Errorf("reload: %w", err)
	}

	status := result["status"]
	fmt.Printf("reload: %s\n", status)
	if errs, ok := result["errors"]; ok {
		for _, e := range errs.([]any) {
			fmt.Printf("  error: %s\n", e)
		}
	}
	return nil
}

func runProjectShow(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var p projectspec.Project
	if err := c.Do("GET", "/api/projects/"+args[0], nil, &p); err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	renderProjectDetail(&p)
	return nil
}

func runProjectBehaviors(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var p projectspec.Project
	if err := c.Do("GET", "/api/projects/"+args[0], nil, &p); err != nil {
		return fmt.Errorf("get project: %w", err)
	}

	renderProjectBehaviors(&p)
	return nil
}

func renderProjectDetail(p *projectspec.Project) {
	fmt.Printf("ID:          %s\n", p.ID)
	fmt.Printf("Name:        %s\n", p.Meta.Name)
	fmt.Printf("WorkDir:     %s\n", p.WorkDir)
	fmt.Printf("WorkspaceID: %s\n", p.WorkspaceID)
	fmt.Printf("CreatedAt:   %s\n", formatTime(p.CreatedAt))
	fmt.Printf("UpdatedAt:   %s\n", formatTime(p.UpdatedAt))

	m := p.Meta

	if len(m.Kits) > 0 {
		fmt.Println("Kits:")
		for _, k := range m.Kits {
			if k.Alias != "" {
				fmt.Printf("  %s (as %s)\n", k.Ref, k.Alias)
			} else {
				fmt.Printf("  %s\n", k.Ref)
			}
		}
	}

	if len(m.Hooks) > 0 {
		fmt.Println("Hooks:")
		for _, h := range m.Hooks {
			requires := ""
			if len(h.Requires) > 0 {
				requires = "  requires=[" + strings.Join(h.Requires, ",") + "]"
			}
			fmt.Printf("  %-30s  on=%s%s\n", h.ID, strings.Join(h.On, ","), requires)
		}
	}

	if len(m.Gates) > 0 {
		fmt.Println("Gates:")
		for _, g := range m.Gates {
			fmt.Printf("  %-30s  on=%s\n", g.ID, strings.Join(g.On, ","))
		}
	}

	if len(m.TaskBehaviors) > 0 {
		fmt.Println("TaskBehaviors:")
		keys := make([]string, 0, len(m.TaskBehaviors))
		for k := range m.TaskBehaviors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-20s  %s\n", k, m.TaskBehaviors[k].Name)
		}
	}

	if len(m.BuiltinCommands) > 0 {
		fmt.Printf("BuiltinCommands: %s\n", strings.Join(m.BuiltinCommands, ", "))
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
		fmt.Printf("%-20s  %s\n", k, b.Name)
		if len(b.Traits) > 0 {
			fmt.Printf("  traits: %s\n", strings.Join(b.Traits, ", "))
		}
		if b.Worktree {
			fmt.Printf("  worktree: true\n")
		}
		if b.Readonly {
			fmt.Printf("  readonly: true\n")
		}
		if b.BranchPrefix != "" {
			fmt.Printf("  branch_prefix: %s\n", b.BranchPrefix)
		}
		if b.BaseBranch != "" {
			fmt.Printf("  base_branch: %s\n", b.BaseBranch)
		}
	}
}
