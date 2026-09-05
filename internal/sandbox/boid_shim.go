package sandbox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/novshi-tech/boid/internal/yamlutil"
	"gopkg.in/yaml.v3"
)

const boidShimUsage = `Usage: boid <command> [subcommand] [flags]

Commands:
  task     Manage tasks (create, show, update, list, notify, answer, ask, wait, delete, import,
           reopen, current, instructions, env, payload, attachments list, attachments get,
           identity link, identity unlink, identity resolve, resolve-or-capture)
  card     Read cards (get, list)
  signal   Scan and ack the signal inbox (list, ack, ingest, cursor)
  job      Manage jobs (done, list, show, log)
  action   Send and list actions (send, list)
  agent    Manage agent (stop)
  project  Inspect projects (list, behaviors)

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

	// A container-backend job only gets BOID_BROKER_TLS_ADDR, never
	// BOID_BROKER_SOCKET; reject only when both are unset.
	if os.Getenv("BOID_BROKER_SOCKET") == "" && os.Getenv("BOID_BROKER_TLS_ADDR") == "" {
		return nil, fmt.Errorf("boid shim: neither BOID_BROKER_SOCKET nor BOID_BROKER_TLS_ADDR is set")
	}

	// boid fetch <url> is dispatched via FetchRequest, not BoidRequest.
	if len(args) > 0 && args[0] == "fetch" {
		return runFetchShim(args[1:])
	}

	// `task current`/`instructions`/`env`/`payload` read BOID_TASK_ID/BOID_JOB_ID
	// from the environment and take their own request/response path.
	if len(args) >= 2 && args[0] == "task" {
		if op, ok := taskContextOps[args[1]]; ok {
			return runTaskContextShim(op, args)
		}
		// `task attachments list`/`get <name>` also take their own path: a
		// positional name and binary (base64) reply don't fit taskContextOps.
		if args[1] == "attachments" {
			return runTaskAttachmentsShim(args[2:])
		}
	}

	req, err := parseBoidRequest(args)
	if err != nil {
		return nil, err
	}

	cwd, _ := os.Getwd()
	execReq := ExecRequest{
		Command: os.Args[0],
		Args:    append([]string(nil), args...),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
		Boid:    req,
	}
	return sendExecRequest(execReq)
}

func runFetchShim(args []string) (*ExecResponse, error) {
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
		Command: os.Args[0],
		Args:    append([]string{"fetch"}, url),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
		Fetch:   &FetchRequest{URL: url},
	}
	return sendExecRequest(execReq)
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
		switch args[1] {
		case "send":
			return parseBoidActionSend(args[2:])
		case "list":
			return parseBoidActionList(args[2:])
		default:
			return nil, fmt.Errorf("boid shim: unsupported boid action subcommand %q", args[1])
		}
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
		case "wait":
			return parseBoidTaskWait(args[2:])
		case "identity":
			if len(args) < 3 {
				return nil, fmt.Errorf("boid shim: missing boid task identity subcommand")
			}
			switch args[2] {
			case "link":
				return parseBoidTaskIdentityLink(args[3:])
			case "unlink":
				return parseBoidTaskIdentityUnlink(args[3:])
			case "resolve":
				return parseBoidTaskIdentityResolve(args[3:])
			default:
				return nil, fmt.Errorf("boid shim: unsupported boid task identity subcommand %q", args[2])
			}
		case "resolve-or-capture":
			return parseBoidTaskResolveOrCapture(args[2:])
		default:
			return nil, fmt.Errorf("boid shim: unsupported boid task subcommand %q", args[1])
		}
	case "project":
		if len(args) < 2 {
			return nil, fmt.Errorf("boid shim: missing boid project subcommand")
		}
		switch args[1] {
		case "behaviors":
			return parseBoidProjectBehaviors(args[2:])
		case "list":
			return parseBoidProjectList(args[2:])
		default:
			return nil, fmt.Errorf("boid shim: unsupported boid project subcommand %q", args[1])
		}
	case "card":
		if len(args) < 2 {
			return nil, fmt.Errorf("boid shim: missing boid card subcommand")
		}
		switch args[1] {
		case "get":
			return parseBoidCardGet(args[2:])
		case "list":
			return parseBoidCardList(args[2:])
		default:
			return nil, fmt.Errorf("boid shim: unsupported boid card subcommand %q", args[1])
		}
	case "signal":
		if len(args) < 2 {
			return nil, fmt.Errorf("boid shim: missing boid signal subcommand")
		}
		switch args[1] {
		case "list":
			return parseBoidSignalList(args[2:])
		case "claim":
			return parseBoidSignalClaim(args[2:])
		case "ack":
			return parseBoidSignalAck(args[2:])
		case "ingest":
			return parseBoidSignalIngest(args[2:])
		case "cursor":
			return parseBoidSignalCursor(args[2:])
		default:
			return nil, fmt.Errorf("boid shim: unsupported boid signal subcommand %q", args[1])
		}
	default:
		return nil, fmt.Errorf("boid shim: unsupported boid subcommand %q", args[0])
	}
}

// parseBoidProjectBehaviors builds the BoidRequest for `boid project
// behaviors <project-ref>`.
func parseBoidProjectBehaviors(args []string) (*BoidRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("boid shim: project behaviors requires a project ref")
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("boid shim: unexpected argument %q for boid project behaviors", args[1])
	}
	return &BoidRequest{Op: BoidOpProjectBehaviors, ProjectID: args[0]}, nil
}

// parseBoidProjectList builds the BoidRequest for `boid project list`, always
// scoped to the caller's own workspace.
func parseBoidProjectList(args []string) (*BoidRequest, error) {
	if len(args) > 0 {
		return nil, fmt.Errorf("boid shim: unexpected argument %q for boid project list", args[0])
	}
	return &BoidRequest{Op: BoidOpProjectList}, nil
}

// parseBoidAgentStop builds the BoidRequest for `boid agent stop <job-id>`,
// asking the daemon to terminate the agent process while the job's runner
// stays alive to complete the job normally. Broker rejects targeting any job
// other than the caller's own.
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
			// Missing file is a silent skip, sending job done with empty output.
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
	idempotencyKey := ""
	idempotencyKeySet := false
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
		case arg == "--idempotency-key" || strings.HasPrefix(arg, "--idempotency-key="):
			// Applied after the spec is parsed so the flag wins over a
			// same-named spec field.
			value, next, err := takeStringFlagValue(args, i, "--idempotency-key")
			if err != nil {
				return nil, err
			}
			i = next
			idempotencyKey = value
			idempotencyKeySet = true
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
	// is forwarded without explicit enumeration; the API server applies its
	// own schema and drops deprecated task-row override keys.
	var v map[string]any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("boid shim: parse task spec: %w", err)
	}
	if v == nil {
		v = make(map[string]any)
	}
	if idempotencyKeySet {
		v["idempotency_key"] = idempotencyKey
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
	// --payload-patch routes to a distinct op (BoidOpTaskUpdatePayloadPatch)
	// with its own merge semantics and JobID-based scoping, so it is parsed
	// by a dedicated function rather than folded into `merged` below.
	for _, arg := range args {
		if arg == "--payload-patch" || strings.HasPrefix(arg, "--payload-patch=") {
			return parseBoidTaskUpdatePayloadPatch(args)
		}
	}

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

// parseBoidTaskUpdatePayloadPatch builds the BoidRequest for `boid task
// update --payload-patch <value>`. Unlike other `task update` flags it is
// JobID-scoped rather than a positional task id, so it must be parsed alone:
// a positional task id or any other `task update` flag alongside it is
// rejected.
//
// value follows curl's `@` convention: a bare value is inline patch content,
// `@<path>` reads a file, and `@-` reads stdin.
func parseBoidTaskUpdatePayloadPatch(args []string) (*BoidRequest, error) {
	var source string
	var seen bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--payload-patch" || strings.HasPrefix(arg, "--payload-patch="):
			value, next, err := takeStringFlagValue(args, i, "--payload-patch")
			if err != nil {
				return nil, err
			}
			i = next
			source = value
			seen = true
		default:
			return nil, fmt.Errorf("boid shim: --payload-patch cannot be combined with %q; call it alone (boid task update --payload-patch @-)", arg)
		}
	}
	if !seen {
		return nil, fmt.Errorf("boid shim: --payload-patch requires a value")
	}

	data, err := readPayloadPatchSource(source)
	if err != nil {
		return nil, err
	}

	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("boid shim: --payload-patch requires non-empty content")
	}

	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("boid shim: parse payload patch: %w", err)
	}
	// yaml.v3 decodes a mapping with a non-string key as
	// map[interface{}]interface{}, which json.Marshal below cannot handle;
	// NormalizeKeys must match the file-based path's normalization so
	// identical content behaves the same via either route.
	v = yamlutil.NormalizeKeys(v)
	patchJSON, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("boid shim: encode payload patch: %w", err)
	}

	jobID := os.Getenv("BOID_JOB_ID")
	if jobID == "" {
		return nil, fmt.Errorf("boid shim: --payload-patch requires BOID_JOB_ID to be set (this command must run inside a dispatched job)")
	}

	return &BoidRequest{
		Op:           BoidOpTaskUpdatePayloadPatch,
		JobID:        jobID,
		PayloadPatch: patchJSON,
	}, nil
}

// readPayloadPatchSource resolves a --payload-patch value following curl's
// `@` convention (bare value = inline content, `@<path>` = file, `@-` =
// stdin), enforcing PayloadPatchMaxBytes on every branch since this content
// crosses the broker RPC boundary into the daemon process. The broker
// re-checks the same cap independently as defense in depth.
func readPayloadPatchSource(source string) ([]byte, error) {
	if !strings.HasPrefix(source, "@") {
		if len(source) > PayloadPatchMaxBytes {
			return nil, fmt.Errorf("boid shim: --payload-patch inline value exceeds %d bytes", PayloadPatchMaxBytes)
		}
		return []byte(source), nil
	}

	path := strings.TrimPrefix(source, "@")
	var reader io.Reader
	if path == "-" {
		reader = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("boid shim: read payload patch: %w", err)
		}
		defer f.Close()
		reader = f
	}

	data, err := io.ReadAll(io.LimitReader(reader, PayloadPatchMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("boid shim: read payload patch: %w", err)
	}
	if len(data) > PayloadPatchMaxBytes {
		return nil, fmt.Errorf("boid shim: --payload-patch content exceeds %d bytes", PayloadPatchMaxBytes)
	}
	return data, nil
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

// parseBoidTaskWait builds the BoidRequest for `boid task wait <task-id>`.
// Takes no flags on purpose — any bound on how long to wait belongs to
// whoever dispatches the job, not to the agent typing the command.
func parseBoidTaskWait(args []string) (*BoidRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("boid shim: task wait requires a task id")
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task wait", args[1])
	}
	return &BoidRequest{Op: BoidOpTaskWait, TaskID: args[0]}, nil
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

// parseBoidCardGet builds the BoidRequest for `boid card get <task-id>`,
// the single-card half of the card read surface. Kept separate from `boid
// task show` because it returns the card's own projection, not
// orchestrator.Task's columns.
func parseBoidCardGet(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpCardGet}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "-"):
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid card get", arg)
		default:
			if req.TaskID != "" {
				return nil, fmt.Errorf("boid shim: unexpected argument %q for boid card get", arg)
			}
			req.TaskID = arg
		}
	}

	if req.TaskID == "" {
		return nil, fmt.Errorf("boid shim: boid card get requires a task id")
	}
	return req, nil
}

// parseBoidCardList builds the BoidRequest for
// `boid card list [--status S] [--project-id P] [--workspace-id W]`, the
// collection half of the card read surface.
func parseBoidCardList(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpCardList}

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
		default:
			return nil, fmt.Errorf("boid shim: unsupported argument %q for boid card list", arg)
		}
	}

	return req, nil
}

// parseBoidSignalList builds the BoidRequest for `boid signal list [--claim]
// [--source <pack>/<connector>] [--state pending|dead|acked|all]
// [--limit N] [--json]`. There is deliberately no --workspace-id flag —
// workspace scoping is broker-injected from the job token. --json is a
// no-op: every sandbox-side list op already replies with JSON.
func parseBoidSignalList(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpSignalList}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--claim":
			req.Claim = true
		case arg == "--json":
			// no-op — see doc comment above.
		case arg == "--source" || strings.HasPrefix(arg, "--source="):
			value, next, err := takeStringFlagValue(args, i, "--source")
			if err != nil {
				return nil, err
			}
			i = next
			req.Connector = value
		case arg == "--state" || strings.HasPrefix(arg, "--state="):
			value, next, err := takeStringFlagValue(args, i, "--state")
			if err != nil {
				return nil, err
			}
			i = next
			req.SignalState = value
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
			return nil, fmt.Errorf("boid shim: unsupported argument %q for boid signal list", arg)
		}
	}

	return req, nil
}

// parseBoidSignalClaim builds the BoidRequest for `boid signal claim
// <id>...`. Positional ids only, same shape as `signal ack`.
func parseBoidSignalClaim(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpSignalClaim}

	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid signal claim", arg)
		}
		req.SignalIDs = append(req.SignalIDs, arg)
	}

	if len(req.SignalIDs) == 0 {
		return nil, fmt.Errorf("boid shim: boid signal claim requires at least one id")
	}
	return req, nil
}

// parseBoidSignalAck builds the BoidRequest for `boid signal ack <id>...`.
// Takes one or more positional ids; AckSignals is idempotent per id, so
// repeating an already-acked id is a no-op success, not an error.
func parseBoidSignalAck(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpSignalAck}

	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid signal ack", arg)
		}
		req.SignalIDs = append(req.SignalIDs, arg)
	}

	if len(req.SignalIDs) == 0 {
		return nil, fmt.Errorf("boid shim: boid signal ack requires at least one id")
	}
	return req, nil
}

// signalConnectorEnv reads the connector's own identity from the
// environment, never a CLI flag, so a connector process cannot address
// another source's inbox rows / cursor.
func signalConnectorEnv() (service, connector string, err error) {
	service = os.Getenv("BOID_SIGNAL_SERVICE")
	connector = os.Getenv("BOID_SIGNAL_CONNECTOR")
	if service == "" || connector == "" {
		return "", "", fmt.Errorf("boid shim: this command requires BOID_SIGNAL_SERVICE and BOID_SIGNAL_CONNECTOR to be set (this command must run inside a connector job)")
	}
	return service, connector, nil
}

// parseBoidSignalIngest builds the BoidRequest for `boid signal ingest`:
// connector-only, takes no arguments, and reads its JSONL body from stdin,
// capped at PayloadPatchMaxBytes (errors on overflow rather than silently
// truncating). Parsing/validating each JSONL line happens server-side in
// the executor, not here.
func parseBoidSignalIngest(args []string) (*BoidRequest, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("boid shim: boid signal ingest takes no arguments (unexpected %q)", args[0])
	}
	service, connector, err := signalConnectorEnv()
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(io.LimitReader(os.Stdin, PayloadPatchMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("boid shim: read signal ingest stdin: %w", err)
	}
	if len(data) > PayloadPatchMaxBytes {
		return nil, fmt.Errorf("boid shim: signal ingest stdin exceeds %d bytes", PayloadPatchMaxBytes)
	}

	return &BoidRequest{
		Op:            BoidOpSignalIngest,
		Service:       service,
		Connector:     connector,
		IngestPayload: data,
	}, nil
}

// parseBoidSignalCursor builds the BoidRequest for `boid signal cursor`:
// connector-only, takes no arguments, returns the caller's own
// (service, connector)'s stored cursor.
func parseBoidSignalCursor(args []string) (*BoidRequest, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("boid shim: boid signal cursor takes no arguments (unexpected %q)", args[0])
	}
	service, connector, err := signalConnectorEnv()
	if err != nil {
		return nil, err
	}
	return &BoidRequest{
		Op:        BoidOpSignalCursorGet,
		Service:   service,
		Connector: connector,
	}, nil
}

// parseBoidTaskIdentityLink builds the BoidRequest for
// `boid task identity link <identity> <task-id> [--project-id P]`.
// project_id is optional — the broker defaults it from the token's own
// context when omitted, exactly like `boid task create`.
func parseBoidTaskIdentityLink(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskIdentityLink}
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--project-id" || strings.HasPrefix(arg, "--project-id="):
			value, next, err := takeStringFlagValue(args, i, "--project-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.ProjectID = value
		case strings.HasPrefix(arg, "-"):
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task identity link", arg)
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) < 2 {
		return nil, fmt.Errorf("boid shim: boid task identity link requires an identity and a task id")
	}
	if len(positional) > 2 {
		return nil, fmt.Errorf("boid shim: unexpected argument %q for boid task identity link", positional[2])
	}
	req.Identity = positional[0]
	req.TaskID = positional[1]
	return req, nil
}

// parseBoidTaskIdentityUnlink builds the BoidRequest for
// `boid task identity unlink <identity> [--project-id P]`.
func parseBoidTaskIdentityUnlink(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskIdentityUnlink}
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--project-id" || strings.HasPrefix(arg, "--project-id="):
			value, next, err := takeStringFlagValue(args, i, "--project-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.ProjectID = value
		case strings.HasPrefix(arg, "-"):
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task identity unlink", arg)
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) == 0 {
		return nil, fmt.Errorf("boid shim: boid task identity unlink requires an identity")
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("boid shim: unexpected argument %q for boid task identity unlink", positional[1])
	}
	req.Identity = positional[0]
	return req, nil
}

// parseBoidTaskIdentityResolve builds the BoidRequest for
// `boid task identity resolve <identity> [--project-id P]`. A miss is
// represented by the executor as a distinct exit code
// (sandbox.IdentityNotFoundExitCode), not a shim-level error.
func parseBoidTaskIdentityResolve(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskIdentityResolve}
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--project-id" || strings.HasPrefix(arg, "--project-id="):
			value, next, err := takeStringFlagValue(args, i, "--project-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.ProjectID = value
		case strings.HasPrefix(arg, "-"):
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task identity resolve", arg)
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) == 0 {
		return nil, fmt.Errorf("boid shim: boid task identity resolve requires an identity")
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("boid shim: unexpected argument %q for boid task identity resolve", positional[1])
	}
	req.Identity = positional[0]
	return req, nil
}

// parseBoidTaskResolveOrCapture builds the BoidRequest for
// `boid task resolve-or-capture <identity> [--title T]
// [--description D | --description-file F] [--project-id P]`.
// Title/description are only used by the executor when Identity is
// unresolved; a caller that only wants to check for an existing binding can
// omit both.
func parseBoidTaskResolveOrCapture(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpTaskResolveOrCapture}
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--project-id" || strings.HasPrefix(arg, "--project-id="):
			value, next, err := takeStringFlagValue(args, i, "--project-id")
			if err != nil {
				return nil, err
			}
			i = next
			req.ProjectID = value
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
		case arg == "--description-file" || strings.HasPrefix(arg, "--description-file="):
			value, next, err := takeStringFlagValue(args, i, "--description-file")
			if err != nil {
				return nil, err
			}
			i = next
			data, err := readFlagContent(value)
			if err != nil {
				return nil, fmt.Errorf("boid shim: read description file: %w", err)
			}
			req.Description = string(data)
		case strings.HasPrefix(arg, "-"):
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task resolve-or-capture", arg)
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) == 0 {
		return nil, fmt.Errorf("boid shim: boid task resolve-or-capture requires an identity")
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("boid shim: unexpected argument %q for boid task resolve-or-capture", positional[1])
	}
	req.Identity = positional[0]
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
// left empty — the broker fills it from the token's current task. The
// broker holds the connection open until the user/supervisor answers.
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

// parseBoidActionList builds the BoidRequest for `boid action list
// [--project-id P] [--workspace-id W] [--task T] [--since CURSOR]
// [--limit N]`. All flags are optional: with none given, the result is
// scoped to the caller's own workspace and starts from the beginning.
func parseBoidActionList(args []string) (*BoidRequest, error) {
	req := &BoidRequest{Op: BoidOpActionList}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
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
		case arg == "--task" || strings.HasPrefix(arg, "--task="):
			value, next, err := takeStringFlagValue(args, i, "--task")
			if err != nil {
				return nil, err
			}
			i = next
			req.TaskID = value
		case arg == "--since" || strings.HasPrefix(arg, "--since="):
			value, next, err := takeStringFlagValue(args, i, "--since")
			if err != nil {
				return nil, err
			}
			i = next
			req.Since = value
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
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid action list", arg)
		}
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
