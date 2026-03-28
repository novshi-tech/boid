package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/model"
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

func init() {
	taskListCmd.Flags().String("status", "", "Filter by status")
	taskCreateCmd.Flags().String("title", "", "Task title (required)")
	taskCreateCmd.Flags().String("project", "", "Project ID (required)")
	taskCreateCmd.Flags().String("behavior", "", "Task behavior (required)")
	taskCmd.AddCommand(taskListCmd, taskCreateCmd, taskShowCmd)
	rootCmd.AddCommand(taskCmd)
}

func runTaskList(cmd *cobra.Command, args []string) error {
	status, _ := cmd.Flags().GetString("status")
	c := client.NewUnixClient(client.DefaultSocketPath())

	path := "/api/tasks"
	if status != "" {
		path += "?status=" + status
	}

	var tasks []model.Task
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

	var task model.Task
	if err := c.Do("POST", "/api/tasks", req, &task); err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	fmt.Printf("task created: %s (%s)\n", task.ID, task.Status)
	return nil
}

func runTaskShow(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())

	var task model.Task
	if err := c.Do("GET", "/api/tasks/"+args[0], nil, &task); err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	fmt.Printf("ID:       %s\n", task.ID)
	fmt.Printf("Project:  %s\n", task.ProjectID)
	fmt.Printf("Title:    %s\n", task.Title)
	fmt.Printf("Status:   %s\n", task.Status)
	fmt.Printf("Behavior: %s\n", task.Behavior)

	if len(task.Payload) > 0 {
		var pretty json.RawMessage
		if json.Unmarshal(task.Payload, &pretty) == nil {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			fmt.Print("Payload:  ")
			enc.Encode(pretty)
		}
	}

	return nil
}
