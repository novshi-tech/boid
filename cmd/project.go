package cmd

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/projectspec"
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

func init() {
	projectCmd.AddCommand(projectAddCmd, projectListCmd, projectRemoveCmd, projectReloadCmd)
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
