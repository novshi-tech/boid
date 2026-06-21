package sandbox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const boidShimUsage = `Usage: boid <command> [subcommand] [flags]

Commands:
  task    Manage tasks (create, show, update, list, notify, answer, ask, delete, import, reopen)
  job     Manage jobs (done, list, show, log)
  action  Send actions (send)
  agent   Manage agent (stop)

Run "boid <command> --help" for subcommand usage.
`

func containsHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

func RunBoidShim(args []string) (*ExecResponse, error) {
	if containsHelpFlag(args) {
		return &ExecResponse{ExitCode: 0, Stdout: boidShimUsage}, nil
	}

	brokerSocket := os.Getenv("BOID_BROKER_SOCKET")
	if brokerSocket == "" {
		return nil, fmt.Errorf("boid shim: BOID_BROKER_SOCKET not set")
	}

	// boid fetch <url> is dispatched via FetchRequest, not BoidRequest.
	if len(args) > 0 && args[0] == "fetch" {
		return runFetchShim(args[1:], brokerSocket)
	}

	req, err := parseBoidRequest(args)
	if err != nil {
		return nil, err
	}

	cwd, _ := os.Getwd()
	execReq := ExecRequest{
		Command: shimBinaryPath(os.Args[0]),
		Args:    append([]string(nil), args...),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
		Boid:    req,
	}
	return sendExecRequest(brokerSocket, execReq)
}

func runFetchShim(args []string, brokerSocket string) (*ExecResponse, error) {
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("boid fetch: URL is required")
	}
	if strings.HasPrefix(args[0], "-") {
		return nil, fmt.Errorf("boid fetch: unsupported flag %q", args[0])
	}
	url := args[0]
	if len(args) > 1 {
		return nil, fmt.Errorf("boid fetch: unexpected argument %q", args[1])
	}

	cwd, _ := os.Getwd()
	execReq := ExecRequest{
		Command: shimBinaryPath(os.Args[0]),
		Args:    append([]string{"fetch"}, url),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
		Fetch:   &FetchRequest{URL: url},
	}
	return sendExecRequest(brokerSocket, execReq)
}

func parseBoidRequest(args []string) (*BoidRequest, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("boid shim: missing subcommand")
	}

	switch args[0] {
	case "action":
		if len(args) < 2 {
			return nil, fmt.Errorf("boid shim: missing boid action subcommand")
		}
		if args[1] != "send" {
			return nil, fmt.Errorf("boid shim: unsupported boid action subcommand %q", args[1])
		}
		return parseBoidActionSend(args[2:])
	case "agent":
		if len(args) < 2 {
			return nil, fmt.Errorf("boid shim: missing boid agent subcommand")
		}
		if args[1] != "stop" {
			return nil, fmt.Errorf("boid shim: unsupported boid agent subcommand %q", args[1])
		}
		return parseBoidAgentStop(args[2:])
	case "job":
		if len(args) < 2 {
			return nil, fmt.Errorf("boid shim: missing boid job subcommand")
		}
		switch args[1] {
		case "done":
			return parseBoidJobDone(args[2:])
		case "list":
			return parseBoidJobList(args[2:])
		case "show":
			return parseBoidJobShow(args[2:])
		case "log":
			return parseBoidJobLog(args[2:])
		default:
			return nil, fmt.Errorf("boid shim: unsupported boid job subcommand %q", args[1])
		}
	case "task":
		switch args[1] {
		case "create":
			return parseBoidTaskCreate(args[2:])
		case "show":
			return parseBoidTaskShow(args[2:])
		case "update":
			return parseBoidTaskUpdate(args[2:])
		case "import":
			return parseBoidTaskImport(args[2:])
		case "reopen":
			return parseBoidTaskReopen(args[2:])
		case "list":
			return parseBoidTaskList(args[2:])
		case "notify":
			return parseBoidTaskNotify(args[2:])
		case "answer":
			return parseBoidTaskAnswer(args[2:])
		case "ask":
			return parseBoidTaskAsk(args[2:])
		case "delete":
			return parseBoidTaskDelete(args[2:])
		default:
			return nil, fmt.Errorf("boid shim: unsupported boid task subcommand %q", args[1])
		}
	default:
		return nil, fmt.Errorf("boid shim: unsupported boid subcommand %q", args[0])
	}
}

// parseBoidAgentStop builds the BoidRequest for `boid agent stop <job-id>`.
// The agent (claude) invokes this to ask the daemon to terminate its own
// claude process while leaving bash and the EXIT trap alive — so the
// trap's `boid job done --output-file payload_patch.json` can complete the
// job through the broker normally with the session id intact. The job
// id is the current job (BOID_JOB_ID); broker rejects calls targeting any
// other job.
func parseBoidAgentStop(args []string) (*BoidRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("boid shim: agent stop requires a job id")
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("boid shim: unsupported argument %q for boid agent stop", args[1])
	}
	return &BoidRequest{
		Op:    BoidOpAgentStop,
		JobID: args[0],
	}, nil
}

func parseBoidJobDone(args []string) (*BoidRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("boid shim: job done requires a job id")
	}

	req := &BoidRequest{
		Op:    BoidOpJobDone,
		JobID: args[0],
	}

	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "--exit-code" || strings.HasPrefix(arg, "--exit-code="):
			value, next, err := takeStringFlagValue(rest, i, "--exit-code")
			if err != nil {
				return nil, err
			}
			i = next
			exitCode, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("boid shim: invalid exit code %q", value)
			}
			req.ExitCode = exitCode
		case arg == "--output-file" || strings.HasPrefix(arg, "--output-file="):
			value, next, err := takeStringFlagValue(rest, i, "--output-file")
			if err != nil {
				return nil, err
			}
			i = next
			// host 側 cmd/job.go runJobDone と挙動を揃える: missing file は
			// silent skip し、output 空で boid job done を送る。
			content, err := readFlagContent(value)
			if err == nil {
				req.Output = string(content)
			}
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid job done", arg)
		}
	}

	return req, nil
}

func parseBoidTaskCreate(args []string) (*BoidRequest, error) {
	filePath := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--file" || strings.HasPrefix(arg, "--file="):
			flagName := "--file"
			if arg == "-f" {
				flagName = "-f"
			}
			value, next, err := takeStringFlagValue(args, i, flagName)
			if err != nil {
				return nil, err
			}
			i = next
			filePath = value
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task create", arg)
		}
	}

	var data []byte
	var err error
	if filePath != "" {
		data, err = os.ReadFile(filePath)
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return nil, fmt.Errorf("boid shim: read task spec: %w", err)
	}

	// Unmarshal the entire YAML spec into a generic map so that every field
	// is forwarded without explicit enumeration. The API server applies its
	// own schema and (per Phase 2-3) silently drops deprecated task-row
	// override keys (readonly / worktree / branch_prefix / base_branch).
	var v map[string]any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("boid shim: parse task spec: %w", err)
	}
	if v == nil {
		v = make(map[string]any)
	}

	title, _ := v["title"].(string)
	if title == "" {
		return nil, fmt.Errorf("boid shim: task spec must include title")
	}
	behavior, _ := v["behavior"].(string)
	if behavior != "" && v["behavior_spec"] != nil {
		return nil, fmt.Errorf("boid shim: task spec must not include both behavior and behavior_spec")
	}

	// Extract project_id for broker authorization (also kept inside CreatePatch
	// for the executor).
	projectID, _ := v["project_id"].(string)

	patchJSON, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("boid shim: encode create patch: %w", err)
	}

	return &BoidRequest{
		Op:          BoidOpTaskCreate,
		ProjectID:   projectID,
		CreatePatch: patchJSON,
	}, nil
}

func parseBoidTaskShow(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskGet}

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		req.TaskID = args[0]
		args = args[1:]
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--field" || strings.HasPrefix(arg, "--field="):
			value, next, err := takeStringFlagValue(args, i, "--field")
			if err != nil {
				return nil, err
			}
			i = next
			req.TaskField = value
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task show", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: task show requires a task id")
	}
	if req.TaskField == "" {
		return nil, fmt.Errorf("boid shim: task show in sandbox requires --field (full detail view is host-only)")
	}

	return req, nil
}

func parseBoidTaskUpdate(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskUpdate}

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		req.TaskID = args[0]
		args = args[1:]
	}

	// merged holds the fields that will become UpdatePatch (JSON of
	// api.UpdateTaskRequest). Individual flags are backward-compat wrappers
	// that write into this map.
	merged := make(map[string]any)

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--patch-file" || strings.HasPrefix(arg, "--patch-file="):
			value, next, err := takeStringFlagValue(args, i, "--patch-file")
			if err != nil {
				return nil, err
			}
			i = next
			data, err := readFlagContent(value)
			if err != nil {
				return nil, fmt.Errorf("boid shim: read patch file: %w", err)
			}
			var base map[string]any
			if err := yaml.Unmarshal(data, &base); err != nil {
				return nil, fmt.Errorf("boid shim: parse patch file: %w", err)
			}
			for k, val := range base {
				merged[k] = val
			}
		case arg == "--title" || strings.HasPrefix(arg, "--title="):
			value, next, err := takeStringFlagValue(args, i, "--title")
			if err != nil {
				return nil, err
			}
			i = next
			merged["title"] = value
		case arg == "--description" || strings.HasPrefix(arg, "--description="):
			value, next, err := takeStringFlagValue(args, i, "--description")
			if err != nil {
				return nil, err
			}
			i = next
			merged["description"] = value
		case arg == "--payload-file" || strings.HasPrefix(arg, "--payload-file="):
			value, next, err := takeStringFlagValue(args, i, "--payload-file")
			if err != nil {
				return nil, err
			}
			i = next
			data, err := readFlagContent(value)
			if err != nil {
				return nil, err
			}
			var v any
			if err := yaml.Unmarshal(data, &v); err != nil {
				return nil, fmt.Errorf("boid shim: parse payload: %w", err)
			}
			merged["payload"] = v
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task update", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: task update requires a task id")
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("boid shim: task update requires at least one of --title, --description, --payload-file, or --patch-file")
	}

	patchJSON, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("boid shim: encode update patch: %w", err)
	}
	req.UpdatePatch = patchJSON

	return req, nil
}

func takeStringFlagValue(args []string, index int, name string) (string, int, error) {
	arg := args[index]
	if strings.HasPrefix(arg, name+"=") {
		return strings.TrimPrefix(arg, name+"="), index, nil
	}
	if index+1 >= len(args) {
		return "", index, fmt.Errorf("boid shim: %s requires a value", name)
	}
	return args[index+1], index + 1, nil
}

func readFlagContent(source string) ([]byte, error) {
	if source == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(source)
}

func parseBoidTaskList(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskList}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--status" || strings.HasPrefix(arg, "--status="):
			value, next, err := takeStringFlagValue(args, i, "--status")
			if err != nil {
				return nil, err
			}
			i = next
			req.Status = value
		case arg == "--project-id" || strings.HasPrefix(arg, "--project-id="):
			value, next, err := takeStringFlagValue(args, i, "--project-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.ProjectID = value
		case arg == "--workspace-id" || strings.HasPrefix(arg, "--workspace-id="):
			value, next, err := takeStringFlagValue(args, i, "--workspace-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.WorkspaceID = value
		case arg == "--limit" || strings.HasPrefix(arg, "--limit="):
			value, next, err := takeStringFlagValue(args, i, "--limit")
			if err != nil {
				return nil, err
			}
			i = next
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("boid shim: invalid limit %q", value)
			}
			req.Limit = n
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task list", arg)
		}
	}

	return req, nil
}

func parseBoidTaskReopen(args []string) (*BoidRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("boid shim: task reopen requires a task id")
	}
	req := &BoidRequest{Op: BoidOpTaskReopen, TaskID: args[0]}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "-m" || arg == "--message" || strings.HasPrefix(arg, "--message="):
			flagName := "--message"
			if arg == "-m" {
				flagName = "-m"
			}
			value, next, err := takeStringFlagValue(rest, i, flagName)
			if err != nil {
				return nil, err
			}
			i = next
			req.Message = value
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task reopen", arg)
		}
	}
	return req, nil
}

func parseBoidTaskNotify(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskNotify}

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		req.TaskID = args[0]
		args = args[1:]
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-m" || arg == "--message" || strings.HasPrefix(arg, "--message="):
			flagName := "--message"
			if arg == "-m" {
				flagName = "-m"
			}
			value, next, err := takeStringFlagValue(args, i, flagName)
			if err != nil {
				return nil, err
			}
			i = next
			req.Message = value
		case arg == "--ask" || strings.HasPrefix(arg, "--ask="):
			value, next, err := takeStringFlagValue(args, i, "--ask")
			if err != nil {
				return nil, err
			}
			i = next
			req.Ask = value
		case arg == "--question-id" || strings.HasPrefix(arg, "--question-id="):
			value, next, err := takeStringFlagValue(args, i, "--question-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.QuestionID = value
		case arg == "--progress" || strings.HasPrefix(arg, "--progress="):
			value, next, err := takeStringFlagValue(args, i, "--progress")
			if err != nil {
				return nil, err
			}
			i = next
			req.Progress = value
		case arg == "--done" || strings.HasPrefix(arg, "--done="):
			value, next, err := takeStringFlagValue(args, i, "--done")
			if err != nil {
				return nil, err
			}
			i = next
			req.Done = value
		case arg == "--fail" || strings.HasPrefix(arg, "--fail="):
			value, next, err := takeStringFlagValue(args, i, "--fail")
			if err != nil {
				return nil, err
			}
			i = next
			req.Fail = value
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task notify", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: task notify requires a task id")
	}
	modes := 0
	for _, m := range []string{req.Ask, req.Progress, req.Done, req.Fail} {
		if m != "" {
			modes++
		}
	}
	if modes > 1 {
		return nil, fmt.Errorf("boid shim: --ask, --progress, --done, --fail are mutually exclusive")
	}
	if req.Message == "" && req.Progress == "" {
		return nil, fmt.Errorf("boid shim: task notify requires --message")
	}

	return req, nil
}

func parseBoidTaskAnswer(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskAnswer}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--task" || strings.HasPrefix(arg, "--task="):
			value, next, err := takeStringFlagValue(args, i, "--task")
			if err != nil {
				return nil, err
			}
			i = next
			req.TaskID = value
		case arg == "--question-id" || strings.HasPrefix(arg, "--question-id="):
			value, next, err := takeStringFlagValue(args, i, "--question-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.QuestionID = value
		case arg == "--answer" || strings.HasPrefix(arg, "--answer="):
			value, next, err := takeStringFlagValue(args, i, "--answer")
			if err != nil {
				return nil, err
			}
			i = next
			req.Answer = value
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task answer", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: task answer requires --task")
	}
	if req.QuestionID == "" {
		return nil, fmt.Errorf("boid shim: task answer requires --question-id")
	}
	if req.Answer == "" {
		return nil, fmt.Errorf("boid shim: task answer requires --answer")
	}

	return req, nil
}

// parseBoidTaskAsk builds the BoidRequest for `boid task ask <question>`.
// The question is the positional argument(s); flags are rejected. TaskID is
// intentionally left empty — the broker fills it from the token's current
// task, so the agent does not have to pass its own id (matching the skill's
// `ANSWER=$(boid task ask "<question>")` form). The broker holds the
// connection open until the user/supervisor answers (no timeout, decision C1).
func parseBoidTaskAsk(args []string) (*BoidRequest, error) {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task ask", a)
		}
		parts = append(parts, a)
	}
	question := strings.TrimSpace(strings.Join(parts, " "))
	if question == "" {
		return nil, fmt.Errorf("boid shim: task ask requires a question")
	}
	return &BoidRequest{Op: BoidOpTaskAsk, Question: question}, nil
}

func parseBoidActionSend(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpActionSend}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--task" || strings.HasPrefix(arg, "--task="):
			value, next, err := takeStringFlagValue(args, i, "--task")
			if err != nil {
				return nil, err
			}
			i = next
			req.TaskID = value
		case arg == "--type" || strings.HasPrefix(arg, "--type="):
			value, next, err := takeStringFlagValue(args, i, "--type")
			if err != nil {
				return nil, err
			}
			i = next
			req.ActionType = value
		case arg == "--payload" || strings.HasPrefix(arg, "--payload="):
			value, next, err := takeStringFlagValue(args, i, "--payload")
			if err != nil {
				return nil, err
			}
			i = next
			data, err := readFlagContent(value)
			if err != nil {
				return nil, fmt.Errorf("boid shim: read payload: %w", err)
			}
			req.Payload = data
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid action send", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: action send requires --task")
	}
	if req.ActionType == "" {
		return nil, fmt.Errorf("boid shim: action send requires --type")
	}

	return req, nil
}

func parseBoidJobList(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpJobList}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--task" || strings.HasPrefix(arg, "--task="):
			value, next, err := takeStringFlagValue(args, i, "--task")
			if err != nil {
				return nil, err
			}
			i = next
			req.TaskID = value
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid job list", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: job list requires --task")
	}

	return req, nil
}

func parseBoidJobShow(args []string) (*BoidRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("boid shim: job show requires a job id")
	}
	return &BoidRequest{Op: BoidOpJobShow, JobID: args[0]}, nil
}

func parseBoidJobLog(args []string) (*BoidRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("boid shim: job log requires a job id")
	}
	return &BoidRequest{Op: BoidOpJobLog, JobID: args[0]}, nil
}

func parseBoidTaskDelete(args []string) (*BoidRequest, error) {
	var taskID string
	var force bool

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--force":
			force = true
		case !strings.HasPrefix(arg, "-") && taskID == "":
			taskID = arg
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task delete", arg)
		}
	}

	if taskID == "" {
		return nil, fmt.Errorf("boid shim: task delete requires a task id")
	}

	return &BoidRequest{Op: BoidOpTaskDelete, TaskID: taskID, Force: force}, nil
}

func parseBoidTaskImport(args []string) (*BoidRequest, error) {
	var filePath string
	var projectOverride string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--file" || strings.HasPrefix(arg, "--file="):
			flagName := "--file"
			if arg == "-f" {
				flagName = "-f"
			}
			value, next, err := takeStringFlagValue(args, i, flagName)
			if err != nil {
				return nil, err
			}
			i = next
			filePath = value
		case arg == "--project" || strings.HasPrefix(arg, "--project="):
			value, next, err := takeStringFlagValue(args, i, "--project")
			if err != nil {
				return nil, err
			}
			i = next
			projectOverride = value
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task import", arg)
		}
	}

	var reader io.Reader
	if filePath != "" {
		f, err := os.Open(filePath)
		if err != nil {
			return nil, fmt.Errorf("boid shim: open import file: %w", err)
		}
		defer f.Close()
		reader = f
	} else {
		reader = os.Stdin
	}

	var tasks []json.RawMessage
	scanner := bufio.NewScanner(reader)
	lineNum := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineNum++
		if !json.Valid([]byte(line)) {
			return nil, fmt.Errorf("boid shim: line %d: invalid JSON: %s", lineNum, line)
		}
		tasks = append(tasks, json.RawMessage(line))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("boid shim: read import input: %w", err)
	}

	if len(tasks) == 0 {
		return nil, fmt.Errorf("boid shim: task import requires at least one task")
	}

	return &BoidRequest{
		Op:                    BoidOpTaskImport,
		ImportTasks:           tasks,
		ImportProjectOverride: projectOverride,
	}, nil
}
