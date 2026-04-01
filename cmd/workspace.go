package cmd

import (
	"fmt"

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
	workspaceCmd.AddCommand(workspaceListCmd, workspaceAssignCmd, workspaceClearCmd)
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

func runWorkspaceClear(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+args[0]+"/workspace", map[string]string{"workspace_id": ""}, &project); err != nil {
		return fmt.Errorf("clear workspace: %w", err)
	}

	fmt.Printf("workspace cleared: %s\n", project.ID)
	return nil
}
