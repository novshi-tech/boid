package cmd

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Manage local workspace groupings",
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured workspaces",
	RunE:  runWorkspaceList,
}

var workspaceShowCmd = &cobra.Command{
	Use:   "show <workspace-id>",
	Short: "Show projects and recent tasks in a workspace",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceShow,
}

var workspaceAssignCmd = &cobra.Command{
	Use:   "assign <project-id> <workspace-id>",
	Short: "Assign a project to a workspace",
	Args:  cobra.ExactArgs(2),
	RunE:  runWorkspaceAssign,
}

var workspaceClearCmd = &cobra.Command{
	Use:   "clear <project-id>",
	Short: "Clear a project's workspace assignment",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceClear,
}

func init() {
	workspaceCmd.AddCommand(workspaceListCmd, workspaceShowCmd, workspaceAssignCmd, workspaceClearCmd)
	rootCmd.AddCommand(workspaceCmd)
}

func runWorkspaceList(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var workspaces []orchestrator.WorkspaceSummary
	if err := c.Do("GET", "/api/workspaces", nil, &workspaces); err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}

	if len(workspaces) == 0 {
		fmt.Println("no workspaces configured")
		return nil
	}

	for _, workspace := range workspaces {
		fmt.Printf("%-20s %d projects\n", workspace.ID, workspace.ProjectCount)
	}
	return nil
}

func runWorkspaceAssign(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+args[0]+"/workspace", map[string]string{"workspace_id": args[1]}, &project); err != nil {
		return fmt.Errorf("assign workspace: %w", err)
	}

	fmt.Printf("workspace assigned: %s -> %s\n", project.ID, project.WorkspaceID)
	return nil
}

func runWorkspaceShow(cmd *cobra.Command, args []string) error {
	workspaceID := args[0]
	c := client.NewUnixClient(client.DefaultSocketPath())

	var projects []*orchestrator.Project
	if err := c.Do("GET", "/api/projects?workspace_id="+workspaceID, nil, &projects); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}

	if len(projects) == 0 {
		fmt.Printf("workspace %s: no projects\n", workspaceID)
		return nil
	}

	fmt.Printf("Workspace: %s\n", workspaceID)
	fmt.Printf("Projects:  %d\n\n", len(projects))

	for _, project := range projects {
		name := filepath.Base(project.WorkDir)
		fmt.Printf("Project: %s  (%s)\n", name, project.ID)

		var tasks []*orchestrator.Task
		if err := c.Do("GET", "/api/tasks?project_id="+project.ID, nil, &tasks); err != nil {
			fmt.Printf("  (failed to list tasks: %v)\n\n", err)
			continue
		}

		sort.Slice(tasks, func(i, j int) bool {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		})
		if len(tasks) > 5 {
			tasks = tasks[:5]
		}

		if len(tasks) == 0 {
			fmt.Println("  (no tasks)")
		} else {
			for _, task := range tasks {
				fmt.Printf("  %-10s %-36s %s\n", task.Status, task.ID, task.Title)
			}
		}
		fmt.Println()
	}
	return nil
}

func runWorkspaceClear(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+args[0]+"/workspace", map[string]string{"workspace_id": ""}, &project); err != nil {
		return fmt.Errorf("clear workspace: %w", err)
	}

	fmt.Printf("workspace cleared: %s\n", project.ID)
	return nil
}
