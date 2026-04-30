package sandbox

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
)

type gitInvocationMode int

const (
	// gitInvocationDirect はネットワーク・remote 設定に触れない自己完結系
	// サブコマンド (commit/merge/rebase/log/diff/cherry-pick/apply 等)。
	// deny-list (clone/pull/remote/submodule) に含まれないサブコマンドはすべて
	// こちらに分類され、broker 側で host の git をそのまま fork する。
	gitInvocationDirect gitInvocationMode = iota
	// gitInvocationBrokered は remote 同期 (push/fetch) を表す。broker が
	// 構造化リクエスト (GitRequest) として受け取り、許可された remote URL と
	// refspec のみを再構築して実行する。
	gitInvocationBrokered
)

type gitInvocation struct {
	mode    gitInvocationMode
	request *GitRequest
}

// deniedGitSubcommands はネットワーク同期・remote 設定変更を起こすため
// broker 経路でも直接実行でも許可しないサブコマンドの deny-list。
// fetch/push は別経路 (brokered) で許可し、config は validateGitConfigArgs で
// キー単位に制御するため、ここには含めない。
var deniedGitSubcommands = map[string]struct{}{
	"clone":     {},
	"pull":      {},
	"remote":    {},
	"submodule": {},
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
	brokerSocket := os.Getenv("BOID_BROKER_SOCKET")
	if brokerSocket == "" {
		return 1, fmt.Errorf("boid shim: BOID_BROKER_SOCKET not set")
	}

	resp, err := shimExecGit(brokerSocket, args, nil)
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

func shimExecGit(brokerSocket string, args []string, gitReq *GitRequest) (*ExecResponse, error) {
	conn, err := net.Dial("unix", brokerSocket)
	if err != nil {
		return nil, fmt.Errorf("connect to broker: %w", err)
	}
	defer conn.Close()

	cwd, _ := os.Getwd()
	token := os.Getenv("BOID_BROKER_TOKEN")
	req := ExecRequest{
		Command: shimBinaryPath(os.Args[0]),
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
		return &gitInvocation{mode: gitInvocationDirect}, nil
	}

	if _, denied := deniedGitSubcommands[subcmd]; denied {
		return nil, fmt.Errorf("git subcommand %q is not allowed", subcmd)
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
	case "config":
		if err := validateGitConfigArgs(rest); err != nil {
			return nil, err
		}
		return &gitInvocation{mode: gitInvocationDirect}, nil
	default:
		return &gitInvocation{mode: gitInvocationDirect}, nil
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
	}
	// --delete フラグまたは : refspec が含まれる場合は push_delete operation に分類
	hasDelete := req.Delete
	for _, refspec := range req.Refspecs {
		if strings.HasPrefix(refspec, ":") {
			hasDelete = true
		}
	}
	if hasDelete {
		req.Op = GitOpPushDelete
	}
	return req, nil
}

// validateGitConfigArgs validates arguments for "git config ...".
// Scope flags (--global, --system, --file, --worktree) are always rejected.
// Read-mode flags (--get, --list, etc.) are always allowed.
// Write-mode calls are allowed only when the key does not match a forbidden prefix.
func validateGitConfigArgs(args []string) error {
	for _, arg := range args {
		switch arg {
		case "--global", "--system", "--worktree":
			return fmt.Errorf("git config %q is not allowed", arg)
		}
		if strings.HasPrefix(arg, "--file") || arg == "-f" {
			return fmt.Errorf("git config %q is not allowed", arg)
		}
		if strings.HasPrefix(arg, "--blob") {
			return fmt.Errorf("git config %q is not allowed", arg)
		}
	}

	// Read-mode flags: permit immediately (scope flags already rejected above).
	for _, arg := range args {
		switch arg {
		case "--get", "--get-all", "--get-regexp", "--get-urlmatch",
			"--list", "-l", "--name-only", "--null", "-z":
			return nil
		}
	}

	// Write or unset: find the key and validate it.
	for i, arg := range args {
		switch arg {
		case "--unset", "--unset-all":
			if i+1 < len(args) {
				return checkForbiddenConfigKey(args[i+1])
			}
			return nil
		case "--add", "--replace-all":
			if i+1 < len(args) {
				return checkForbiddenConfigKey(args[i+1])
			}
			return nil
		case "--remove-section":
			if i+1 < len(args) {
				return checkForbiddenConfigSection(args[i+1])
			}
			return nil
		case "--rename-section":
			if i+1 < len(args) {
				return checkForbiddenConfigSection(args[i+1])
			}
			return nil
		}
		if !strings.HasPrefix(arg, "-") {
			return checkForbiddenConfigKey(arg)
		}
	}
	return nil
}

// checkForbiddenConfigKey rejects writes to sensitive git config keys.
func checkForbiddenConfigKey(key string) error {
	k := strings.ToLower(key)
	if strings.HasPrefix(k, "remote.") {
		for _, suffix := range []string{".url", ".pushurl", ".fetch", ".push"} {
			if strings.HasSuffix(k, suffix) {
				return fmt.Errorf("git config key %q is not allowed", key)
			}
		}
		return nil
	}
	switch k {
	case "core.hookspath", "core.sshcommand", "core.editor", "core.attributesfile":
		return fmt.Errorf("git config key %q is not allowed", key)
	}
	for _, prefix := range []string{"filter.", "url.", "credential.", "include.", "includeif."} {
		if strings.HasPrefix(k, prefix) {
			return fmt.Errorf("git config key %q is not allowed", key)
		}
	}
	return nil
}

// checkForbiddenConfigSection rejects --remove-section / --rename-section for
// sections that contain forbidden keys.
func checkForbiddenConfigSection(section string) error {
	s := strings.ToLower(section)
	for _, prefix := range []string{"filter.", "url.", "credential.", "include.", "includeif."} {
		if strings.HasPrefix(s, prefix) || s == strings.TrimSuffix(prefix, ".") {
			return fmt.Errorf("git config section %q is not allowed", section)
		}
	}
	// remote.* section contains url/pushurl/fetch/push keys → block.
	if strings.HasPrefix(s, "remote.") || s == "remote" {
		return fmt.Errorf("git config section %q is not allowed", section)
	}
	return nil
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
			case "--delete", "-D":
				req.Delete = true
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

