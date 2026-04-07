package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
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

var taskDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskDelete,
}

func init() {
	taskListCmd.Flags().String("status", "", "Filter by status")
	taskCreateCmd.Flags().StringP("file", "f", "", "YAML file to read task spec from (default: stdin)")
	taskWatchCmd.Flags().Duration("interval", time.Second, "Polling interval")
	taskGetCmd.Flags().String("field", "", "Field name to retrieve (required)")
	taskDeleteCmd.Flags().Bool("force", false, "Delete even if task is active")
	taskCmd.AddCommand(taskListCmd, taskCreateCmd, taskShowCmd, taskWatchCmd, taskGetCmd, taskDeleteCmd)
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

// taskCreateSpec is the YAML schema for task create input.
type taskCreateSpec struct {
	ProjectID   string         `yaml:"project_id"`
	Title       string         `yaml:"title"`
	Description string         `yaml:"description,omitempty"`
	Behavior    string         `yaml:"behavior"`
	AutoStart   bool           `yaml:"auto_start,omitempty"`
	Payload     map[string]any `yaml:"payload,omitempty"`
}

func runTaskCreate(cmd *cobra.Command, args []string) error {
	filePath, _ := cmd.Flags().GetString("file")

	var r io.Reader
	if filePath != "" {
		f, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("open file: %w", err)
		}
		defer f.Close()
		r = f
	} else {
		r = cmd.InOrStdin()
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	var spec taskCreateSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}

	if spec.ProjectID == "" || spec.Title == "" || spec.Behavior == "" {
		return fmt.Errorf("YAML must include project_id, title, and behavior")
	}

	req := map[string]any{
		"project_id": spec.ProjectID,
		"title":      spec.Title,
		"behavior":   spec.Behavior,
	}
	if spec.Description != "" {
		req["description"] = spec.Description
	}
	if spec.AutoStart {
		req["auto_start"] = spec.AutoStart
	}
	if spec.Payload != nil {
		payloadJSON, err := json.Marshal(spec.Payload)
		if err != nil {
			return fmt.Errorf("encode payload: %w", err)
		}
		req["payload"] = json.RawMessage(payloadJSON)
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

func runTaskDelete(cmd *cobra.Command, args []string) error {
	force, _ := cmd.Flags().GetBool("force")
	c := client.NewUnixClient(client.DefaultSocketPath())

	path := "/api/tasks/" + args[0]
	if force {
		path += "?force=true"
	}
	if err := c.Do("DELETE", path, nil, nil); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	fmt.Printf("task deleted: %s\n", args[0])
	return nil
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
