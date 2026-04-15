package sandbox

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const realGitPath = "/usr/bin/git"

type gitInvocationMode int

const (
	gitInvocationLocal gitInvocationMode = iota
	gitInvocationBrokered
)

type gitInvocation struct {
	mode    gitInvocationMode
	request *GitRequest
}

var localGitSubcommands = map[string]struct{}{
	"add":       {},
	"branch":    {},
	"checkout":  {},
	"commit":    {},
	"diff":      {},
	"help":      {},
	"log":       {},
	"ls-files":  {},
	"merge":     {},
	"mv":        {},
	"rebase":    {},
	"reset":     {},
	"restore":   {},
	"rev-parse": {},
	"rm":        {},
	"show":      {},
	"stash":     {},
	"status":    {},
	"switch":    {},
	"tag":       {},
	"worktree":  {},
}

var allowedGitGlobalOptions = map[string]struct{}{
	"-P":                   {},
	"-h":                   {},
	"--help":               {},
	"--no-pager":           {},
	"--no-replace-objects": {},
	"--paginate":           {},
	"--version":            {},
	"-p":                   {},
}

func RunGitShim(args []string) (int, error) {
	invocation, err := classifyGitInvocation(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, nil
	}
	if invocation.mode == gitInvocationLocal {
		return runRealGit(args)
	}

	brokerSocket := os.Getenv("BOID_BROKER_SOCKET")
	if brokerSocket == "" {
		return 1, fmt.Errorf("boid shim: BOID_BROKER_SOCKET not set")
	}

	resp, err := shimExecGit(brokerSocket, args, invocation.request)
	if err != nil {
		return 1, fmt.Errorf("boid shim: %w", err)
	}
	if resp.Stdout != "" {
		_, _ = os.Stdout.WriteString(resp.Stdout)
	}
	if resp.Stderr != "" {
		_, _ = os.Stderr.WriteString(resp.Stderr)
	}
	return resp.ExitCode, nil
}

func runRealGit(args []string) (int, error) {
	cmd := exec.Command(realGitPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

func shimExecGit(brokerSocket string, args []string, gitReq *GitRequest) (*ExecResponse, error) {
	conn, err := net.Dial("unix", brokerSocket)
	if err != nil {
		return nil, fmt.Errorf("connect to broker: %w", err)
	}
	defer conn.Close()

	cwd, _ := os.Getwd()
	token := os.Getenv("BOID_BROKER_TOKEN")
	req := ExecRequest{
		Command: "git",
		Args:    append([]string(nil), args...),
		Cwd:     cwd,
		Token:   token,
		Git:     gitReq,
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	var resp ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return &resp, nil
}

func classifyGitInvocation(args []string) (*gitInvocation, error) {
	subcmd, rest, err := splitGitArgs(args)
	if err != nil {
		return nil, err
	}
	if subcmd == "" {
		return &gitInvocation{mode: gitInvocationLocal}, nil
	}
	if _, ok := localGitSubcommands[subcmd]; ok {
		return &gitInvocation{mode: gitInvocationLocal}, nil
	}

	switch subcmd {
	case string(GitOpFetch):
		req, err := parseGitFetchRequest(rest)
		if err != nil {
			return nil, err
		}
		return &gitInvocation{mode: gitInvocationBrokered, request: req}, nil
	case string(GitOpPush):
		req, err := parseGitPushRequest(rest)
		if err != nil {
			return nil, err
		}
		return &gitInvocation{mode: gitInvocationBrokered, request: req}, nil
	default:
		return nil, fmt.Errorf("git subcommand %q is not allowed", subcmd)
	}
}

func splitGitArgs(args []string) (string, []string, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return "", nil, fmt.Errorf("git global argument %q is not allowed", arg)
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return arg, args[i+1:], nil
		}
		if isDeniedGitGlobalOption(arg) {
			return "", nil, fmt.Errorf("git global option %q is not allowed", arg)
		}
		if _, ok := allowedGitGlobalOptions[arg]; ok {
			continue
		}
		return "", nil, fmt.Errorf("git global option %q is not allowed", arg)
	}
	return "", nil, nil
}

func isDeniedGitGlobalOption(arg string) bool {
	switch arg {
	case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env":
		return true
	default:
		return strings.HasPrefix(arg, "--git-dir=") ||
			strings.HasPrefix(arg, "--work-tree=") ||
			strings.HasPrefix(arg, "--namespace=") ||
			strings.HasPrefix(arg, "--config-env=")
	}
}

func parseGitFetchRequest(args []string) (*GitRequest, error) {
	req := &GitRequest{Op: GitOpFetch}
	positional, err := parseGitSyncFlags(req, GitOpFetch, args)
	if err != nil {
		return nil, err
	}
	if len(positional) > 0 {
		req.Remote = positional[0]
		req.Refspecs = append(req.Refspecs, positional[1:]...)
	}
	for _, value := range req.Refspecs {
		if strings.HasPrefix(value, "-") {
			return nil, fmt.Errorf("git fetch options must appear before the remote name")
		}
	}
	return req, nil
}

func parseGitPushRequest(args []string) (*GitRequest, error) {
	req := &GitRequest{Op: GitOpPush}
	positional, err := parseGitSyncFlags(req, GitOpPush, args)
	if err != nil {
		return nil, err
	}
	if len(positional) > 0 {
		req.Remote = positional[0]
		req.Refspecs = append(req.Refspecs, positional[1:]...)
	}
	for _, value := range req.Refspecs {
		if strings.HasPrefix(value, "-") {
			return nil, fmt.Errorf("git push options must appear before the remote name")
		}
	}
	for _, refspec := range req.Refspecs {
		if strings.HasPrefix(refspec, "+") {
			return nil, fmt.Errorf("git push force refspecs are not allowed")
		}
		if strings.HasPrefix(refspec, ":") {
			return nil, fmt.Errorf("git push delete refspecs are not allowed")
		}
	}
	return req, nil
}

func parseGitSyncFlags(req *GitRequest, op GitOp, args []string) ([]string, error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(positional) > 0 || !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, args[i:]...)
			break
		}
		switch op {
		case GitOpFetch:
			switch arg {
			case "--dry-run", "-n":
				req.DryRun = true
			case "--verbose", "-v":
				req.Verbose = true
			case "--quiet", "-q":
				req.Quiet = true
			case "--prune", "-p":
				req.Prune = true
			case "--tags", "-t":
				req.Tags = true
			case "--force", "-f":
				req.Force = true
			default:
				return nil, fmt.Errorf("git fetch option %q is not allowed", arg)
			}
		case GitOpPush:
			switch arg {
			case "--dry-run", "-n":
				req.DryRun = true
			case "--verbose", "-v":
				req.Verbose = true
			case "--quiet", "-q":
				req.Quiet = true
			case "--porcelain":
				req.Porcelain = true
			case "--force-with-lease":
				req.ForceWithLease = true
			default:
				if strings.HasPrefix(arg, "--force-with-lease=") {
					req.ForceWithLease = true
					continue
				}
				return nil, fmt.Errorf("git push option %q is not allowed", arg)
			}
		}
	}
	return positional, nil
}

func realGitBinary() string {
	return filepath.Clean(realGitPath)
}
