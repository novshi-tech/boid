package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Manage tasks",
}

var taskListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tasks",
	RunE:  runTaskList,
}

var taskCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a task",
	RunE:  runTaskCreate,
}

var taskShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show task details",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskShow,
}

var taskWatchCmd = &cobra.Command{
	Use:   "watch <id>",
	Short: "Watch task progress",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskWatch,
}

func init() {
	taskListCmd.Flags().String("status", "", "Filter by status")
	taskCreateCmd.Flags().String("title", "", "Task title (required)")
	taskCreateCmd.Flags().String("project", "", "Project ID (required)")
	taskCreateCmd.Flags().String("behavior", "", "Task behavior (required)")
	taskWatchCmd.Flags().Duration("interval", time.Second, "Polling interval")
	taskCmd.AddCommand(taskListCmd, taskCreateCmd, taskShowCmd, taskWatchCmd)
	rootCmd.AddCommand(taskCmd)
}

func runTaskList(cmd *cobra.Command, args []string) error {
	status, _ := cmd.Flags().GetString("status")
	c := client.NewUnixClient(client.DefaultSocketPath())

	path := "/api/tasks"
	if status != "" {
		path += "?status=" + status
	}

	var tasks []orchestrator.Task
	if err := c.Do("GET", path, nil, &tasks); err != nil {
		return err
	}

	if len(tasks) == 0 {
		fmt.Println("no tasks")
		return nil
	}

	for _, t := range tasks {
		fmt.Printf("%-36s %-12s %s\n", t.ID, t.Status, t.Title)
	}
	return nil
}

func runTaskCreate(cmd *cobra.Command, args []string) error {
	title, _ := cmd.Flags().GetString("title")
	projectID, _ := cmd.Flags().GetString("project")
	behavior, _ := cmd.Flags().GetString("behavior")

	if title == "" || projectID == "" || behavior == "" {
		return fmt.Errorf("--title, --project, and --behavior are required")
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	req := map[string]string{
		"project_id": projectID,
		"title":      title,
		"behavior":   behavior,
	}

	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks", req, &task); err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	fmt.Printf("task created: %s (%s)\n", task.ID, task.Status)
	return nil
}

func runTaskShow(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var detail api.TaskDetailView
	if err := c.Do("GET", "/api/tasks/"+args[0]+"/detail", nil, &detail); err != nil {
		return fmt.Errorf("get task detail: %w", err)
	}

	return renderTaskDetail(&detail)
}

func runTaskWatch(cmd *cobra.Command, args []string) error {
	interval, _ := cmd.Flags().GetDuration("interval")
	if interval <= 0 {
		return fmt.Errorf("--interval must be positive")
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	taskID := args[0]
	var lastFingerprint string

	for {
		var detail api.TaskDetailView
		if err := c.Do("GET", "/api/tasks/"+taskID+"/detail", nil, &detail); err != nil {
			return fmt.Errorf("watch task: %w", err)
		}

		data, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("snapshot task detail: %w", err)
		}
		snapshot := string(data)
		if snapshot != lastFingerprint {
			printWatchHeader("task", detail.Task.ID)
			if err := renderTaskDetail(&detail); err != nil {
				return err
			}
			fmt.Println()
			lastFingerprint = snapshot
		}

		if isTerminalTaskStatus(detail.Task.Status) {
			return nil
		}
		time.Sleep(interval)
	}
}
