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

	return renderOutput(cmd, workspaces, func() error {
		if len(workspaces) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no workspaces configured")
			return nil
		}
		for _, workspace := range workspaces {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %d projects\n", workspace.ID, workspace.ProjectCount)
		}
		return nil
	})
}

func runWorkspaceAssign(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+args[0]+"/workspace", map[string]string{"workspace_id": args[1]}, &project); err != nil {
		return fmt.Errorf("assign workspace: %w", err)
	}

	return renderOutput(cmd, &project, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace assigned: %s -> %s\n", project.ID, project.WorkspaceID)
		return nil
	})
}

func runWorkspaceShow(cmd *cobra.Command, args []string) error {
	workspaceID := args[0]
	c := client.NewUnixClient(client.DefaultSocketPath())

	var projects []*orchestrator.Project
	if err := c.Do("GET", "/api/projects?workspace_id="+workspaceID, nil, &projects); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}

	type projectEntry struct {
		ID    string             `json:"id"    yaml:"id"`
		Name  string             `json:"name"  yaml:"name"`
		Tasks []*orchestrator.Task `json:"tasks" yaml:"tasks"`
	}
	type workspaceView struct {
		WorkspaceID  string         `json:"workspace_id"  yaml:"workspace_id"`
		ProjectCount int            `json:"project_count" yaml:"project_count"`
		Projects     []projectEntry `json:"projects"      yaml:"projects"`
	}

	view := workspaceView{
		WorkspaceID:  workspaceID,
		ProjectCount: len(projects),
		Projects:     make([]projectEntry, 0, len(projects)),
	}

	for _, project := range projects {
		var tasks []*orchestrator.Task
		if err := c.Do("GET", "/api/tasks?project_id="+project.ID, nil, &tasks); err != nil {
			tasks = nil
		}
		sort.Slice(tasks, func(i, j int) bool {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		})
		if len(tasks) > 5 {
			tasks = tasks[:5]
		}
		view.Projects = append(view.Projects, projectEntry{
			ID:    project.ID,
			Name:  filepath.Base(project.WorkDir),
			Tasks: tasks,
		})
	}

	return renderOutput(cmd, view, func() error {
		out := cmd.OutOrStdout()
		if len(projects) == 0 {
			fmt.Fprintf(out, "workspace %s: no projects\n", workspaceID)
			return nil
		}
		fmt.Fprintf(out, "Workspace: %s\n", workspaceID)
		fmt.Fprintf(out, "Projects:  %d\n\n", len(projects))
		for _, entry := range view.Projects {
			fmt.Fprintf(out, "Project: %s  (%s)\n", entry.Name, entry.ID)
			if len(entry.Tasks) == 0 {
				fmt.Fprintln(out, "  (no tasks)")
			} else {
				for _, task := range entry.Tasks {
					fmt.Fprintf(out, "  %-10s %-36s %s\n", task.Status, task.ID, task.Title)
				}
			}
			fmt.Fprintln(out)
		}
		return nil
	})
}

func runWorkspaceClear(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var project orchestrator.Project
	if err := c.Do("PUT", "/api/projects/"+args[0]+"/workspace", map[string]string{"workspace_id": ""}, &project); err != nil {
		return fmt.Errorf("clear workspace: %w", err)
	}

	return renderOutput(cmd, &project, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "workspace cleared: %s\n", project.ID)
		return nil
	})
}
