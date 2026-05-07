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

func RunBoidShim(args []string) (*ExecResponse, error) {
	brokerSocket := os.Getenv("BOID_BROKER_SOCKET")
	if brokerSocket == "" {
		return nil, fmt.Errorf("boid shim: BOID_BROKER_SOCKET not set")
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

func parseBoidRequest(args []string) (*BoidRequest, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("boid shim: missing subcommand")
	}

	switch args[0] {
	case "job":
		if args[1] != "done" {
			return nil, fmt.Errorf("boid shim: unsupported boid job subcommand %q", args[1])
		}
		return parseBoidJobDone(args[2:])
	case "task":
		switch args[1] {
		case "create":
			return parseBoidTaskCreate(args[2:])
		case "get":
			return parseBoidTaskGet(args[2:])
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
		default:
			return nil, fmt.Errorf("boid shim: unsupported boid task subcommand %q", args[1])
		}
	default:
		return nil, fmt.Errorf("boid shim: unsupported boid subcommand %q", args[0])
	}
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

	var spec struct {
		ProjectID    string         `yaml:"project_id"`
		Title        string         `yaml:"title"`
		Description  string         `yaml:"description"`
		Behavior     string         `yaml:"behavior"`
		BehaviorSpec *struct {
			Name           string         `yaml:"name"`
			Traits         []string       `yaml:"traits,omitempty"`
			Readonly       bool           `yaml:"readonly,omitempty"`
			Worktree       bool           `yaml:"worktree,omitempty"`
			BranchPrefix   string         `yaml:"branch_prefix,omitempty"`
			BaseBranch     string         `yaml:"base_branch,omitempty"`
			DefaultPayload map[string]any `yaml:"default_payload,omitempty"`
		} `yaml:"behavior_spec"`
		Payload          map[string]any `yaml:"payload"`
		Ref              string         `yaml:"ref"`
		ParentID         string         `yaml:"parent_id"`
		DependsOn        []string       `yaml:"depends_on"`
		DependsOnPayload string         `yaml:"depends_on_payload"`
		AutoStart        bool           `yaml:"auto_start"`
		BaseBranch       string         `yaml:"base_branch"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("boid shim: parse task spec: %w", err)
	}

	if spec.Title == "" {
		return nil, fmt.Errorf("boid shim: task spec must include title")
	}
	if spec.Behavior != "" && spec.BehaviorSpec != nil {
		return nil, fmt.Errorf("boid shim: task spec must not include both behavior and behavior_spec")
	}
	// behavior 省略時はサーバ側で DefaultBehavior に routing される。

	req := &BoidRequest{
		Op:               BoidOpTaskCreate,
		ProjectID:        spec.ProjectID,
		Title:            spec.Title,
		Description:      spec.Description,
		Behavior:         spec.Behavior,
		BaseBranch:       spec.BaseBranch,
		Ref:              spec.Ref,
		ParentID:         spec.ParentID,
		DependsOn:        spec.DependsOn,
		DependsOnPayload: spec.DependsOnPayload,
		AutoStart:        spec.AutoStart,
	}
	if spec.BehaviorSpec != nil {
		bs := &BehaviorSpec{
			Name:         spec.BehaviorSpec.Name,
			Traits:       spec.BehaviorSpec.Traits,
			Readonly:     spec.BehaviorSpec.Readonly,
			Worktree:     spec.BehaviorSpec.Worktree,
			BranchPrefix: spec.BehaviorSpec.BranchPrefix,
			BaseBranch:   spec.BehaviorSpec.BaseBranch,
		}
		if spec.BehaviorSpec.DefaultPayload != nil {
			dpJSON, err := json.Marshal(spec.BehaviorSpec.DefaultPayload)
			if err != nil {
				return nil, fmt.Errorf("boid shim: encode behavior_spec.default_payload: %w", err)
			}
			bs.DefaultPayload = dpJSON
		}
		req.BehaviorSpec = bs
	}
	if spec.Payload != nil {
		payloadJSON, err := json.Marshal(spec.Payload)
		if err != nil {
			return nil, fmt.Errorf("boid shim: encode payload: %w", err)
		}
		req.Payload = payloadJSON
	}

	return req, nil
}

func parseBoidTaskGet(args []string) (*BoidRequest, error) {
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
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task get", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: task get requires a task id")
	}
	if req.TaskField == "" {
		return nil, fmt.Errorf("boid shim: task get requires --field")
	}

	return req, nil
}

func parseBoidTaskUpdate(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskUpdate}

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		req.TaskID = args[0]
		args = args[1:]
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--title" || strings.HasPrefix(arg, "--title="):
			value, next, err := takeStringFlagValue(args, i, "--title")
			if err != nil {
				return nil, err
			}
			i = next
			req.Title = value
		case arg == "--description" || strings.HasPrefix(arg, "--description="):
			value, next, err := takeStringFlagValue(args, i, "--description")
			if err != nil {
				return nil, err
			}
			i = next
			req.Description = value
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
			// payload ファイルは YAML/JSON 両対応 (cmd/task.go update と同等)
			var v any
			if err := yaml.Unmarshal(data, &v); err != nil {
				return nil, fmt.Errorf("boid shim: parse payload: %w", err)
			}
			payloadJSON, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("boid shim: encode payload: %w", err)
			}
			req.Payload = payloadJSON
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task update", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: task update requires a task id")
	}
	if req.Title == "" && req.Description == "" && len(req.Payload) == 0 {
		return nil, fmt.Errorf("boid shim: task update requires at least one of --title, --description, or --payload-file")
	}

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
	return &BoidRequest{Op: BoidOpTaskReopen, TaskID: args[0]}, nil
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
		case arg == "--session-id" || strings.HasPrefix(arg, "--session-id="):
			value, next, err := takeStringFlagValue(args, i, "--session-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.SessionID = value
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task notify", arg)
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: task notify requires a task id")
	}
	if req.Message == "" {
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

func parseBoidTaskImport(args []string) (*BoidRequest, error) {
	var filePath string
	var projectOverride string
	var datasourceOverride string

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
		case arg == "--datasource" || strings.HasPrefix(arg, "--datasource="):
			value, next, err := takeStringFlagValue(args, i, "--datasource")
			if err != nil {
				return nil, err
			}
			i = next
			datasourceOverride = value
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
		Op:                      BoidOpTaskImport,
		ImportTasks:             tasks,
		ImportProjectOverride:   projectOverride,
		ImportDatasourceOverride: datasourceOverride,
	}, nil
}
