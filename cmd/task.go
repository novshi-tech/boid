package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
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
	Short: "Show task details, or a single field with --field",
	Long: "Without --field, shows the full task detail view.\n" +
		"With --field <path>, prints just the value at that dotted path:\n" +
		"  - top-level fields: id, title, status, parent_id, ...\n" +
		"  - payload traits (auto-prefixed): awaiting.question, artifact.report, ...\n" +
		"  - computed lifecycle: lifecycle.abort.message, lifecycle.executed",
	Args: cobra.ExactArgs(1),
	RunE: runTaskShow,
}

var taskWatchCmd = &cobra.Command{
	Use:   "watch <id>",
	Short: "Watch task progress",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskWatch,
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
	Short: "Reopen a done task back into executing",
	Long: "done 済みタスクを executing に戻す。 --message を渡すと、 任意のテキストを新しい\n" +
		"instruction として履歴に追記する (agent / model / interactive は前回 active を継承)。\n" +
		"主な用途: PR review feedback を反映させる、 task.exit gate がコンフリクトで失敗した\nPR を修正させる、 等",
	Args: cobra.ExactArgs(1),
	RunE: runTaskReopen,
}

var taskRerunCmd = &cobra.Command{
	Use:   "rerun <id>",
	Short: "Reset a done/aborted task to pending for re-execution with the same ID",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskRerun,
}

var taskNotifyCmd = &cobra.Command{
	Use:   "notify <id>",
	Short: "Send a user notification for the given task",
	Long: "タスクのライフサイクル signal を発信する。 排他的な 4 モード:\n" +
		"  --ask      ユーザに判断を求める Q&A モード。 awaiting に遷移。\n" +
		"  --done     成功報告。 done に遷移。 親 supervisor が verify + (必要なら) reopen。\n" +
		"  --fail     失敗報告。 aborted に遷移。 親 supervisor が reopen / 受容 / abort。\n" +
		"  --progress 進捗ノート (timeline 行のみ、 状態遷移なし)。\n" +
		"いずれも指定しなければ FYI 通知のみ (root task は notify hook が発火、 child task は無音)。\n" +
		"--question-id を省略した場合は boid 側で UUID を生成する。",
	Args: cobra.ExactArgs(1),
	RunE: runTaskNotify,
}

var taskAnswerCmd = &cobra.Command{
	Use:   "answer",
	Short: "Submit a user answer to a pending Q&A question",
	Long: "awaiting 状態のタスクに回答を送る。 --task / --question-id / --answer はすべて必須。\n" +
		"回答が保存されると タスクは awaiting → executing に遷移する。",
	RunE: runTaskAnswer,
}

func init() {
	taskListCmd.Flags().String("status", "", "Filter by status")
	taskListCmd.Flags().String("workspace", "", "Filter by workspace ID")
	taskListCmd.Flags().String("behavior", "", "Filter by behavior name")
	taskListCmd.Flags().Bool("has-depends-on", false, "Show only tasks that have depends_on")
	taskListCmd.Flags().Bool("no-depends-on", false, "Show only tasks that have no depends_on")
	taskCreateCmd.Flags().StringP("file", "f", "", "YAML file to read task spec from (default: stdin)")
	taskWatchCmd.Flags().Duration("interval", time.Second, "Polling interval")
	taskShowCmd.Flags().String("field", "", "Dotted path to a single field (e.g. status, payload.artifact.report, awaiting.question, lifecycle.abort.message). Prints the value as plain text.")
	taskDeleteCmd.Flags().Bool("force", false, "Delete even if task is active")
	taskImportCmd.Flags().StringP("file", "f", "", "JSONL file to import (default: stdin)")
	taskImportCmd.Flags().String("project", "", "Override project_id for all tasks (id or name, partial match supported)")
	taskImportCmd.Flags().String("datasource", "", "Override datasource_id for all tasks")
	taskUpdateCmd.Flags().StringP("patch-file", "f", "", "Patch file (YAML/JSON) with task fields to update; - for stdin")
	taskUpdateCmd.Flags().String("payload-file", "", "Payload file (YAML/JSON) merged into task.payload; - for stdin")
	taskUpdateCmd.Flags().String("instructions-file", "", "Instructions file (YAML/JSON) for role-wise merge; - for stdin")
	taskReopenCmd.Flags().StringP("message", "m", "", "Append a new instruction with the given message (agent/model/interactive are inherited from the active entry)")
	taskDuplicateCmd.Flags().Bool("auto-start", false, "Automatically start the duplicated task")
	taskRerunCmd.Flags().Bool("auto-start", false, "Automatically start the rerun task")
	taskRerunCmd.Flags().String("instructions-file", "", "Instructions override file (YAML/JSON) for role-wise merge; - for stdin")
	taskNotifyCmd.Flags().StringP("message", "m", "", "Notification message text (required for FYI/ask/done/fail modes)")
	taskNotifyCmd.Flags().String("ask", "", "Question text; transitions task to awaiting (Q&A mode)")
	taskNotifyCmd.Flags().String("question-id", "", "Q&A turn ID (generated when omitted)")
	taskNotifyCmd.Flags().String("session-id", "", "Agent session ID stored in awaiting trait and surfaced as BOID_AGENT_SESSION_ID on next hook invocation")
	taskNotifyCmd.Flags().String("progress", "", "Progress message; records timeline Action only — no hook fires, no state change")
	taskNotifyCmd.Flags().String("done", "", "Success summary (one-line headline); transitions task to done. Parent supervisor verifies and optionally reopens.")
	taskNotifyCmd.Flags().String("fail", "", "Failure summary (one-line headline); transitions task to aborted. Parent supervisor inspects and optionally reopens with a fix.")
	taskAnswerCmd.Flags().String("task", "", "Task ID (required)")
	taskAnswerCmd.Flags().String("question-id", "", "Question ID to answer (required)")
	taskAnswerCmd.Flags().String("answer", "", "Answer text (required)")
	taskCmd.AddCommand(taskListCmd, taskCreateCmd, taskShowCmd, taskWatchCmd, taskDeleteCmd, taskUpdateCmd, taskImportCmd, taskDuplicateCmd, taskReopenCmd, taskRerunCmd, taskNotifyCmd, taskAnswerCmd)
	rootCmd.AddCommand(taskCmd)
}

func runTaskUpdate(cmd *cobra.Command, args []string) error {
	patchFile, _ := cmd.Flags().GetString("patch-file")
	payloadFile, _ := cmd.Flags().GetString("payload-file")
	instructionsFile, _ := cmd.Flags().GetString("instructions-file")

	if patchFile == "" && payloadFile == "" && instructionsFile == "" {
		return fmt.Errorf("at least one of --patch-file, --payload-file, or --instructions-file is required")
	}

	var req api.UpdateTaskRequest
	if patchFile != "" {
		data, err := readYAMLAsJSON(cmd, patchFile)
		if err != nil {
			return fmt.Errorf("patch: %w", err)
		}
		if err := json.Unmarshal(data, &req); err != nil {
			return fmt.Errorf("patch: decode into UpdateTaskRequest: %w", err)
		}
	}

	if payloadFile != "" {
		data, err := readYAMLAsJSON(cmd, payloadFile)
		if err != nil {
			return fmt.Errorf("payload: %w", err)
		}
		req.Payload = data
	}

	if instructionsFile != "" {
		data, err := readYAMLAsJSON(cmd, instructionsFile)
		if err != nil {
			return fmt.Errorf("instructions: %w", err)
		}
		req.Instructions = data
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	task, err := c.UpdateTask(args[0], req)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}

	return renderOutput(cmd, task, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "task updated: %s (%s)\n", task.ID, task.Status)
		return nil
	})
}

// readYAMLAsJSON reads a YAML/JSON file (or stdin if path is "-") and
// returns its content as canonical JSON bytes. Intended for CLI flags
// that accept user-authored YAML but need to be sent to the API as JSON.
func readYAMLAsJSON(cmd *cobra.Command, path string) (json.RawMessage, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", path, err)
	}
	return json.RawMessage(encoded), nil
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

	return renderOutput(cmd, tasks, func() error {
		if len(tasks) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no tasks")
			return nil
		}
		for _, t := range tasks {
			fmt.Fprintf(cmd.OutOrStdout(), "%-36s %-12s %s\n", t.ID, t.Status, t.Title)
		}
		return nil
	})
}

// deprecatedTaskRowSpecFields enumerates the task-row override keys that
// Phase 2-3 removed. Specs that still carry them are accepted (the keys are
// stripped and a warning is printed) so legacy YAML on disk keeps working.
var deprecatedTaskRowSpecFields = []string{"readonly", "worktree", "branch_prefix", "base_branch"}

// parseTaskCreateSpec decodes a YAML/JSON task spec into api.CreateTaskRequest.
// The intermediate YAML→JSON conversion is what lets api.CreateTaskRequest's
// json tags drive the schema (yaml tags are intentionally absent there to keep
// a single source of truth). Unknown fields are rejected to surface typos.
//
// As of Phase 2-3, four task-row override keys (readonly / worktree /
// branch_prefix / base_branch) are silently dropped (with a stderr warning)
// before strict decoding so legacy specs do not break.
func parseTaskCreateSpec(data []byte) (api.CreateTaskRequest, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return api.CreateTaskRequest{}, fmt.Errorf("parse YAML: %w", err)
	}
	// Strip the deprecated task-row override keys from the top-level map.
	// Only emit a warning when the key actually appears in the spec.
	if m, ok := raw.(map[string]any); ok {
		for _, key := range deprecatedTaskRowSpecFields {
			if _, present := m[key]; present {
				fmt.Fprintf(os.Stderr,
					"warning: task spec field %q is deprecated and ignored; behavior type and project defaults now control this value\n",
					key,
				)
				delete(m, key)
			}
		}
	}
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return api.CreateTaskRequest{}, fmt.Errorf("encode YAML as JSON: %w", err)
	}
	var req api.CreateTaskRequest
	dec := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return api.CreateTaskRequest{}, fmt.Errorf("decode task spec: %w", err)
	}
	return req, nil
}

// runTaskCreate reads a YAML or JSON task spec from --file (or stdin) and POSTs
// it to the API. behavior may be omitted; the server defaults to
// api.DefaultBehavior in that case.
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

	req, err := parseTaskCreateSpec(data)
	if err != nil {
		return err
	}

	if req.ProjectID == "" {
		req.ProjectID = os.Getenv("BOID_PROJECT_ID")
	}
	if req.ParentID == "" {
		req.ParentID = os.Getenv("BOID_TASK_ID")
	}
	if req.ProjectID == "" || req.Title == "" {
		return fmt.Errorf("YAML must include project_id and title")
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks", req, &task); err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	return renderOutput(cmd, &task, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "task created: %s (%s)\n", task.ID, task.Status)
		return nil
	})
}

func runTaskShow(cmd *cobra.Command, args []string) error {
	field, _ := cmd.Flags().GetString("field")
	c := client.NewUnixClient(client.DefaultSocketPath())

	if field != "" {
		path := "/api/tasks/" + args[0] + "/field?path=" + url.QueryEscape(field)
		status, body, err := c.GetRaw(path)
		if err != nil {
			return fmt.Errorf("get task field: %w", err)
		}
		if status >= 400 {
			msg := strings.TrimSpace(string(body))
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", status)
			}
			return fmt.Errorf("get task field: %s", msg)
		}
		_, _ = cmd.OutOrStdout().Write(body)
		return nil
	}

	var detail api.TaskDetailView
	if err := c.Do("GET", "/api/tasks/"+args[0]+"/detail", nil, &detail); err != nil {
		return fmt.Errorf("get task detail: %w", err)
	}

	return renderOutput(cmd, &detail, func() error {
		return renderTaskDetail(&detail)
	})
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
			if err := renderOutput(cmd, &detail, func() error {
				return renderTaskDetail(&detail)
			}); err != nil {
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
	return renderOutput(cmd, map[string]any{"id": args[0], "deleted": true}, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "task deleted: %s\n", args[0])
		return nil
	})
}

func runTaskImport(cmd *cobra.Command, args []string) error {
	filePath, _ := cmd.Flags().GetString("file")
	projectRef, _ := cmd.Flags().GetString("project")
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

	c := client.NewUnixClient(client.DefaultSocketPath())

	projectID := projectRef
	if projectRef != "" {
		p, err := resolveProjectRef(c, os.Stdin, cmd.OutOrStdout(), projectRef)
		if err != nil {
			return fmt.Errorf("resolve project: %w", err)
		}
		projectID = p.ID
	}

	reqs = applyImportFlags(reqs, projectID, datasourceID)

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
	message, _ := cmd.Flags().GetString("message")

	req := api.ApplyActionRequest{Type: "reopen"}
	if message != "" {
		payload, err := json.Marshal(map[string]any{
			"instruction": map[string]any{
				"message": message,
			},
		})
		if err != nil {
			return fmt.Errorf("marshal instruction: %w", err)
		}
		req.Payload = payload
	}

	result, err := c.ApplyAction(args[0], req)
	if err != nil {
		return fmt.Errorf("reopen task: %w", err)
	}
	return renderOutput(cmd, result, func() error {
		fmt.Fprintf(cmd.OutOrStdout(), "task reopened: %s (%s)\n", result.Task.ID, result.Task.Status)
		return nil
	})
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

	return renderOutput(cmd, &task, func() error {
		fmt.Fprintln(cmd.OutOrStdout(), task.ID)
		return nil
	})
}

func runTaskRerun(cmd *cobra.Command, args []string) error {
	autoStart, _ := cmd.Flags().GetBool("auto-start")
	instructionsFile, _ := cmd.Flags().GetString("instructions-file")
	c := client.NewUnixClient(client.DefaultSocketPath())

	req := api.RerunTaskRequest{AutoStart: autoStart}
	if instructionsFile != "" {
		data, err := readYAMLAsJSON(cmd, instructionsFile)
		if err != nil {
			return fmt.Errorf("instructions: %w", err)
		}
		req.InstructionsOverride = data
	}

	var task orchestrator.Task
	if err := c.Do("POST", "/api/tasks/"+args[0]+"/rerun", req, &task); err != nil {
		return fmt.Errorf("rerun task: %w", err)
	}

	return renderOutput(cmd, &task, func() error {
		fmt.Fprintln(cmd.OutOrStdout(), task.ID)
		return nil
	})
}

func runTaskNotify(cmd *cobra.Command, args []string) error {
	message, _ := cmd.Flags().GetString("message")
	ask, _ := cmd.Flags().GetString("ask")
	progress, _ := cmd.Flags().GetString("progress")
	done, _ := cmd.Flags().GetString("done")
	fail, _ := cmd.Flags().GetString("fail")
	questionID, _ := cmd.Flags().GetString("question-id")
	sessionID, _ := cmd.Flags().GetString("session-id")

	modes := 0
	for _, m := range []string{ask, progress, done, fail} {
		if m != "" {
			modes++
		}
	}
	if modes > 1 {
		return fmt.Errorf("--ask, --progress, --done, --fail are mutually exclusive")
	}
	if message == "" && progress == "" {
		return fmt.Errorf("--message is required")
	}

	c := client.NewUnixClient(client.DefaultSocketPath())
	req := api.NotifyTaskRequest{
		Message:    message,
		Ask:        ask,
		QuestionID: questionID,
		SessionID:  sessionID,
		Progress:   progress,
		Done:       done,
		Fail:       fail,
	}
	if err := c.Do("POST", "/api/tasks/"+args[0]+"/notify", req, nil); err != nil {
		return fmt.Errorf("notify task: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "notified: %s\n", args[0])
	return nil
}

func runTaskAnswer(cmd *cobra.Command, args []string) error {
	taskID, _ := cmd.Flags().GetString("task")
	if taskID == "" {
		return fmt.Errorf("--task is required")
	}
	questionID, _ := cmd.Flags().GetString("question-id")
	if questionID == "" {
		return fmt.Errorf("--question-id is required")
	}
	answer, _ := cmd.Flags().GetString("answer")
	if answer == "" {
		return fmt.Errorf("--answer is required")
	}
	c := client.NewUnixClient(client.DefaultSocketPath())
	req := api.AnswerTaskRequest{QuestionID: questionID, Answer: answer}
	if err := c.Do("POST", "/api/tasks/"+taskID+"/answer", req, nil); err != nil {
		return fmt.Errorf("answer task: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "answered: %s\n", taskID)
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
