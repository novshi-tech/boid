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

var taskGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get a single field from a task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskGet,
}

func init() {
	taskListCmd.Flags().String("status", "", "Filter by status")
	taskCreateCmd.Flags().String("title", "", "Task title (required)")
	taskCreateCmd.Flags().String("project", "", "Project ID (required)")
	taskCreateCmd.Flags().String("behavior", "", "Task behavior (required)")
	taskCreateCmd.Flags().String("payload", "", "Initial payload JSON (optional)")
	taskWatchCmd.Flags().Duration("interval", time.Second, "Polling interval")
	taskGetCmd.Flags().String("field", "", "Field name to retrieve (required)")
	taskCmd.AddCommand(taskListCmd, taskCreateCmd, taskShowCmd, taskWatchCmd, taskGetCmd)
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
	payloadStr, _ := cmd.Flags().GetString("payload")

	if title == "" || projectID == "" || behavior == "" {
		return fmt.Errorf("--title, --project, and --behavior are required")
	}

	req := map[string]any{
		"project_id": projectID,
		"title":      title,
		"behavior":   behavior,
	}
	if payloadStr != "" {
		var payload json.RawMessage
		if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
			return fmt.Errorf("invalid --payload JSON: %w", err)
		}
		req["payload"] = payload
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
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

func runTaskGet(cmd *cobra.Command, args []string) error {
	field, _ := cmd.Flags().GetString("field")
	if field == "" {
		return fmt.Errorf("--field is required")
	}

	c := client.NewUnixClient(client.DefaultSocketPath())

	var task orchestrator.Task
	if err := c.Do("GET", "/api/tasks/"+args[0], nil, &task); err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	switch field {
	case "title":
		fmt.Print(task.Title)
	case "description":
		fmt.Print(task.Description)
	case "status":
		fmt.Print(task.Status)
	default:
		return fmt.Errorf("unknown field %q (supported: title, description, status)", field)
	}
	return nil
}
