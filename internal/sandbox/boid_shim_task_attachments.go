package sandbox

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// runTaskAttachmentsShim dispatches the two Phase 5b PR2 attachments
// subcommands (docs/plans/phase5-shim-and-task-context.md):
// `boid task attachments list` and `boid task attachments get <name>`.
// Unlike the four Phase 5b PR1 task-context subcommands (which share a
// single request/response shape via taskContextOps/runTaskContextShim),
// `get` takes a positional attachment name and an optional `--output`
// flag, and its reply carries base64-encoded bytes rather than JSON/YAML —
// different enough on both ends to warrant its own small dispatcher rather
// than folding into taskContextOps.
//
// subArgs is everything after "task attachments" (RunBoidShim's args[2:]).
func runTaskAttachmentsShim(subArgs []string, brokerSocket string) (*ExecResponse, error) {
	if len(subArgs) == 0 {
		return nil, fmt.Errorf("boid shim: missing boid task attachments subcommand")
	}
	switch subArgs[0] {
	case "list":
		return runTaskAttachmentsList(subArgs[1:], brokerSocket)
	case "get":
		return runTaskAttachmentsGet(subArgs[1:], brokerSocket)
	default:
		return nil, fmt.Errorf("boid shim: unsupported boid task attachments subcommand %q", subArgs[0])
	}
}

// runTaskAttachmentsList builds and sends the BoidOpTaskAttachmentsList
// request, then — for a successful reply — re-renders the broker's
// canonical-JSON array as YAML when requested (defaulting to yaml, mirroring
// runTaskContextShim's --format convention).
func runTaskAttachmentsList(args []string, brokerSocket string) (*ExecResponse, error) {
	format, err := parseAttachmentsListFlags(args)
	if err != nil {
		return nil, err
	}

	req := &BoidRequest{
		Op:     BoidOpTaskAttachmentsList,
		TaskID: os.Getenv("BOID_TASK_ID"),
	}
	cwd, _ := os.Getwd()
	execReq := ExecRequest{
		Command: os.Args[0],
		Args:    append([]string{"task", "attachments", "list"}, args...),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
		Boid:    req,
	}
	resp, err := sendExecRequest(brokerSocket, execReq)
	if err != nil || resp == nil {
		return resp, err
	}
	if format == "yaml" && resp.ExitCode == 0 {
		resp.Stdout = jsonToYAMLForShim(resp.Stdout)
	}
	return resp, nil
}

func parseAttachmentsListFlags(args []string) (format string, err error) {
	format = "yaml"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--format" || strings.HasPrefix(arg, "--format="):
			value, next, ferr := takeStringFlagValue(args, i, "--format")
			if ferr != nil {
				return "", ferr
			}
			i = next
			if value != "json" && value != "yaml" {
				return "", fmt.Errorf("boid shim: --format must be \"json\" or \"yaml\" (got %q)", value)
			}
			format = value
		default:
			return "", fmt.Errorf("boid shim: unsupported flag %q for boid task attachments list", arg)
		}
	}
	return format, nil
}

// runTaskAttachmentsGet builds and sends the BoidOpTaskAttachmentsGet
// request, base64-decodes a successful reply's Stdout (the broker's binary
// transport encoding — see boid_executor.go's BoidOpTaskAttachmentsGet
// case), and either writes the decoded bytes to --output or hands them back
// as ExecResponse.Stdout for main.go's os.Stdout.WriteString to emit
// unmodified (a Go string is just a byte slice, so binary content survives
// even though the field is typed string).
func runTaskAttachmentsGet(args []string, brokerSocket string) (*ExecResponse, error) {
	name, outputPath, err := parseAttachmentsGetArgs(args)
	if err != nil {
		return nil, err
	}

	req := &BoidRequest{
		Op:             BoidOpTaskAttachmentsGet,
		TaskID:         os.Getenv("BOID_TASK_ID"),
		AttachmentName: name,
	}
	cwd, _ := os.Getwd()
	execReq := ExecRequest{
		Command: os.Args[0],
		Args:    append([]string{"task", "attachments", "get"}, args...),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
		Boid:    req,
	}
	resp, err := sendExecRequest(brokerSocket, execReq)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.ExitCode != 0 {
		// An error message in Stderr is never base64 — pass it through
		// unmodified, exactly like the task-context ops' error path.
		return resp, nil
	}

	decoded, decodeErr := base64.StdEncoding.DecodeString(resp.Stdout)
	if decodeErr != nil {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid shim: malformed attachment payload: %v", decodeErr)}, nil
	}

	if outputPath != "" {
		if err := os.WriteFile(outputPath, decoded, 0o644); err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid shim: write %s: %v", outputPath, err)}, nil
		}
		return &ExecResponse{Stdout: fmt.Sprintf("wrote %s (%d bytes)\n", outputPath, len(decoded))}, nil
	}
	return &ExecResponse{Stdout: string(decoded)}, nil
}

// parseAttachmentsGetArgs parses `boid task attachments get [--output
// <path>] [--] <name>`. A literal "--" (the standard POSIX end-of-flags
// marker, same convention as git/grep) forces every argument after it to
// be treated as positional, never a flag — codex review on PR #798 (Minor
// 1) flagged that without this, an attachment legitimately named with a
// leading dash (e.g. "-shot.png", which SanitizeAttachmentName's upload-time
// validator has no objection to) was permanently unreachable via this CLI,
// even though it was present and listed by `boid task attachments list`.
func parseAttachmentsGetArgs(args []string) (name, outputPath string, err error) {
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !positionalOnly && arg == "--" {
			positionalOnly = true
			continue
		}
		switch {
		case !positionalOnly && (arg == "--output" || strings.HasPrefix(arg, "--output=")):
			value, next, ferr := takeStringFlagValue(args, i, "--output")
			if ferr != nil {
				return "", "", ferr
			}
			i = next
			outputPath = value
		case !positionalOnly && strings.HasPrefix(arg, "-"):
			return "", "", fmt.Errorf("boid shim: unsupported flag %q for boid task attachments get", arg)
		default:
			if name != "" {
				return "", "", fmt.Errorf("boid shim: unexpected argument %q for boid task attachments get", arg)
			}
			name = arg
		}
	}
	if name == "" {
		return "", "", fmt.Errorf("boid shim: boid task attachments get requires an attachment name")
	}
	return name, outputPath, nil
}
