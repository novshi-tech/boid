package sandbox

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
		Command: "boid",
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
		if args[1] != "create" {
			return nil, fmt.Errorf("boid shim: unsupported boid task subcommand %q", args[1])
		}
		return parseBoidTaskCreate(args[2:])
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
			content, err := readFlagContent(value)
			if err != nil {
				return nil, err
			}
			req.Output = string(content)
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid job done", arg)
		}
	}

	return req, nil
}

func parseBoidTaskCreate(args []string) (*BoidRequest, error) {
	req := &BoidRequest{
		Op: BoidOpTaskCreate,
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--project" || strings.HasPrefix(arg, "--project="):
			value, next, err := takeStringFlagValue(args, i, "--project")
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
		case arg == "--behavior" || strings.HasPrefix(arg, "--behavior="):
			value, next, err := takeStringFlagValue(args, i, "--behavior")
			if err != nil {
				return nil, err
			}
			i = next
			req.Behavior = value
		case arg == "--description" || strings.HasPrefix(arg, "--description="):
			value, next, err := takeStringFlagValue(args, i, "--description")
			if err != nil {
				return nil, err
			}
			i = next
			req.Description = value
		case arg == "--payload" || strings.HasPrefix(arg, "--payload="):
			value, next, err := takeStringFlagValue(args, i, "--payload")
			if err != nil {
				return nil, err
			}
			i = next
			content, err := readFlagContent(value)
			if err != nil {
				return nil, err
			}
			req.Payload = content
		default:
			return nil, fmt.Errorf("boid shim: unsupported flag %q for boid task create", arg)
		}
	}

	if req.Title == "" {
		return nil, fmt.Errorf("boid shim: task create requires --title")
	}
	if req.Behavior == "" {
		return nil, fmt.Errorf("boid shim: task create requires --behavior")
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
