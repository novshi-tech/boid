package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
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

var taskUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update a task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskUpdate,
}

var taskImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import tasks from JSONL file or stdin",
	RunE:  runTaskImport,
}

var taskDuplicateCmd = &cobra.Command{
	Use:   "duplicate <source_id>",
	Short: "Duplicate a task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskDuplicate,
}

var taskReopenCmd = &cobra.Command{
	Use:   "reopen <id>",
	Short: "Return a done task to reworking",
	Long:  "done 済みタスクを reworking に戻す。\n主な用途: github-auto-merge kit がマージコンフリクトを検出した PR を修正させる",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskReopen,
}

var taskRerunCmd = &cobra.Command{
	Use:   "rerun <id>",
	Short: "Reset a done/aborted task to pending for re-execution with the same ID",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskRerun,
}

func init() {
	taskListCmd.Flags().String("status", "", "Filter by status")
	taskListCmd.Flags().String("workspace", "", "Filter by workspace ID")
	taskListCmd.Flags().String("behavior", "", "Filter by behavior name")
	taskListCmd.Flags().Bool("has-depends-on", false, "Show only tasks that have depends_on")
	taskListCmd.Flags().Bool("no-depends-on", false, "Show only tasks that have no depends_on")
	taskCreateCmd.Flags().StringP("file", "f", "", "YAML file to read task spec from (default: stdin)")
	taskWatchCmd.Flags().Duration("interval", time.Second, "Polling interval")
	taskGetCmd.Flags().String("field", "", "Field name to retrieve (required)")
	taskDeleteCmd.Flags().Bool("force", false, "Delete even if task is active")
	taskImportCmd.Flags().StringP("file", "f", "", "JSONL file to import (default: stdin)")
	taskImportCmd.Flags().String("project", "", "Override project_id for all tasks")
	taskImportCmd.Flags().String("datasource", "", "Override datasource_id for all tasks")
	taskUpdateCmd.Flags().String("title", "", "New title")
	taskUpdateCmd.Flags().String("description", "", "New description")
	taskUpdateCmd.Flags().String("payload-file", "", "Payload file (YAML/JSON), - for stdin")
	taskDuplicateCmd.Flags().Bool("auto-start", false, "Automatically start the duplicated task")
	taskRerunCmd.Flags().Bool("auto-start", false, "Automatically start the rerun task")
	taskCmd.AddCommand(taskListCmd, taskCreateCmd, taskShowCmd, taskWatchCmd, taskGetCmd, taskDeleteCmd, taskUpdateCmd, taskImportCmd, taskDuplicateCmd, taskReopenCmd, taskRerunCmd)
	rootCmd.AddCommand(taskCmd)
}

func runTaskUpdate(cmd *cobra.Command, args []string) error {
	title, _ := cmd.Flags().GetString("title")
	description, _ := cmd.Flags().GetString("description")
	payloadFile, _ := cmd.Flags().GetString("payload-file")

	if title == "" && description == "" && payloadFile == "" {
		return fmt.Errorf("at least one of --title, --description, or --payload-file is required")
	}

	req := api.UpdateTaskRequest{
		Title:       title,
		Description: description,
	}

	if payloadFile != "" {
		var data []byte
		var err error
		if payloadFile == "-" {
			data, err = io.ReadAll(cmd.InOrStdin())
		} else {
			data, err = os.ReadFile(payloadFile)
		}
		if err != nil {
			return fmt.Errorf("read payload file: %w", err)
		}
		var v any
		if err := yaml.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("parse payload: %w", err)
		}
		payloadJSON, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode payload: %w", err)
		}
		req.Payload = json.RawMessage(payloadJSON)
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	task, err := c.UpdateTask(args[0], req)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "task updated: %s (%s)\n", task.ID, task.Status)
	return nil
}

func runTaskList(cmd *cobra.Command, args []string) error {
	status, _ := cmd.Flags().GetString("status")
	workspace, _ := cmd.Flags().GetString("workspace")
	behavior, _ := cmd.Flags().GetString("behavior")
	hasDependsOn, _ := cmd.Flags().GetBool("has-depends-on")
	noDependsOn, _ := cmd.Flags().GetBool("no-depends-on")

	c := client.NewUnixClient(client.DefaultSocketPath())

	var params []string
	if status != "" {
		params = append(params, "status="+status)
	}
	if workspace != "" {
		params = append(params, "workspace_id="+workspace)
	}
	if behavior != "" {
		params = append(params, "behavior="+behavior)
	}
	if hasDependsOn {
		params = append(params, "has_depends_on=true")
	}
	if noDependsOn {
		params = append(params, "no_depends_on=true")
	}

	path := "/api/tasks"
	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}

	var tasks []orchestrator.Task
	if err := c.Do("GET", path, nil, &tasks); err != nil {
		return err
	}

	if len(tasks) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no tasks")
		return nil
	}

	for _, t := range tasks {
		fmt.Fprintf(cmd.OutOrStdout(), "%-36s %-12s %s\n", t.ID, t.Status, t.Title)
	}
	return nil
}

// taskCreateSpec is the YAML schema for task create input.
type taskCreateSpec struct {
	ID               string                     `yaml:"id,omitempty"`
	ProjectID        string                     `yaml:"project_id"`
	Title            string                     `yaml:"title"`
	Description      string                     `yaml:"description,omitempty"`
	Behavior         string                     `yaml:"behavior,omitempty"`
	BehaviorSpec     *orchestrator.BehaviorSpec `yaml:"behavior_spec,omitempty"`
	AutoStart        bool                       `yaml:"auto_start,omitempty"`
	Payload          map[string]any             `yaml:"payload,omitempty"`
	DependsOn        []string                   `yaml:"depends_on,omitempty"`
	DependsOnPayload string                     `yaml:"depends_on_payload,omitempty"`
	Ref              string                     `yaml:"ref,omitempty"`
	ParentID         string                     `yaml:"parent_id,omitempty"`
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

	if spec.ProjectID == "" || spec.Title == "" {
		return fmt.Errorf("YAML must include project_id and title")
	}
	if spec.Behavior == "" && spec.BehaviorSpec == nil {
		return fmt.Errorf("YAML must include either behavior or behavior_spec")
	}
	if spec.Behavior != "" && spec.BehaviorSpec != nil {
		return fmt.Errorf("YAML must not include both behavior and behavior_spec")
	}

	req := map[string]any{
		"project_id": spec.ProjectID,
		"title":      spec.Title,
	}
	if spec.Behavior != "" {
		req["behavior"] = spec.Behavior
	}
	if spec.BehaviorSpec != nil {
		req["behavior_spec"] = spec.BehaviorSpec
	}
	if spec.ID != "" {
		req["id"] = spec.ID
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
	if len(spec.DependsOn) > 0 {
		req["depends_on"] = spec.DependsOn
	}
	if spec.DependsOnPayload != "" {
		req["depends_on_payload"] = spec.DependsOnPayload
	}
	if spec.Ref != "" {
		req["ref"] = spec.Ref
	}
	if spec.ParentID != "" {
		req["parent_id"] = spec.ParentID
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

func runTaskImport(cmd *cobra.Command, args []string) error {
	filePath, _ := cmd.Flags().GetString("file")
	projectID, _ := cmd.Flags().GetString("project")
	datasourceID, _ := cmd.Flags().GetString("datasource")

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

	reqs, err := parseImportLines(r)
	if err != nil {
		return err
	}

	reqs = applyImportFlags(reqs, projectID, datasourceID)

	c := client.NewUnixClient(client.DefaultSocketPath())
	var result api.ImportResult
	if err := c.Do("POST", "/api/tasks/import", reqs, &result); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Created: %d, Skipped: %d, Errors: %d\n",
		result.Created, result.Skipped, len(result.Errors))

	for _, e := range result.Errors {
		fmt.Fprintf(cmd.ErrOrStderr(), "error line %d (remote_id=%s): %s\n",
			e.Line, e.RemoteID, e.Error)
	}
	return nil
}

func runTaskReopen(cmd *cobra.Command, args []string) error {
	c := client.NewUnixClient(client.DefaultSocketPath())
	result, err := c.ApplyAction(args[0], api.ApplyActionRequest{Type: "reopen"})
	if err != nil {
		return fmt.Errorf("reopen task: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "task reopened: %s (%s)\n", result.Task.ID, result.Task.Status)
	return nil
}

func runTaskDuplicate(cmd *cobra.Command, args []string) error {
	autoStart, _ := cmd.Flags().GetBool("auto-start")
	c := client.NewUnixClient(client.DefaultSocketPath())

	req := map[string]any{
		"auto_start": autoStart,
	}

	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks/"+args[0]+"/duplicate", req, &task); err != nil {
		return fmt.Errorf("duplicate task: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), task.ID)
	return nil
}

func runTaskRerun(cmd *cobra.Command, args []string) error {
	autoStart, _ := cmd.Flags().GetBool("auto-start")
	c := client.NewUnixClient(client.DefaultSocketPath())

	req := map[string]any{
		"auto_start": autoStart,
	}

	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks/"+args[0]+"/rerun", req, &task); err != nil {
		return fmt.Errorf("rerun task: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), task.ID)
	return nil
}

func parseImportLines(r io.Reader) ([]api.CreateTaskRequest, error) {
	var reqs []api.CreateTaskRequest
	scanner := bufio.NewScanner(r)
	lineNum := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineNum++
		var req api.CreateTaskRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			return nil, fmt.Errorf("line %d: invalid JSON: %w", lineNum, err)
		}
		reqs = append(reqs, req)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	return reqs, nil
}

func applyImportFlags(reqs []api.CreateTaskRequest, projectID, datasourceID string) []api.CreateTaskRequest {
	for i := range reqs {
		if projectID != "" {
			reqs[i].ProjectID = projectID
		}
		if datasourceID != "" {
			reqs[i].DataSourceID = datasourceID
		}
	}
	return reqs
}
