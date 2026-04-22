package sandbox

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type GitBinding struct {
	ProjectDir     string
	WorktreeRoot   string
	SnapshotBranch string
	Upstream       GitUpstream
	Remotes        map[string]GitRemote
}

type GitUpstream struct {
	Remote   string
	MergeRef string
}

type GitRemote struct {
	FetchURL string
	PushURL  string
}

const realGitPath = "/usr/bin/git"

func realGitBinary() string {
	return filepath.Clean(realGitPath)
}

var gitRepoLocks sync.Map

func captureGitBinding(projectDir, worktreeDir string) (*GitBinding, error) {
	worktreeRoot := worktreeDir
	if worktreeRoot == "" {
		worktreeRoot = projectDir
	}
	if worktreeRoot == "" {
		return nil, fmt.Errorf("git builtin requires project or worktree directory")
	}

	binding := &GitBinding{
		ProjectDir:   projectDir,
		WorktreeRoot: worktreeRoot,
		Remotes:      make(map[string]GitRemote),
	}

	if branch, err := gitOutput(worktreeRoot, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		binding.SnapshotBranch = branch
	}
	if upstream, err := gitOutput(worktreeRoot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		parts := strings.SplitN(upstream, "/", 2)
		if len(parts) == 2 {
			binding.Upstream = GitUpstream{
				Remote:   parts[0],
				MergeRef: "refs/heads/" + parts[1],
			}
		}
	}

	remoteLines, err := gitOutput(worktreeRoot, "config", "--get-regexp", "^remote\\..*\\.(url|pushurl)$")
	if err != nil {
		return binding, nil
	}
	for _, line := range strings.Split(remoteLines, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], parts[1]
		if !strings.HasPrefix(key, "remote.") {
			continue
		}
		key = strings.TrimPrefix(key, "remote.")
		dot := strings.LastIndex(key, ".")
		if dot <= 0 || dot == len(key)-1 {
			continue
		}
		name := key[:dot]
		field := key[dot+1:]
		remote := binding.Remotes[name]
		switch field {
		case "url":
			remote.FetchURL = value
		case "pushurl":
			remote.PushURL = value
		}
		binding.Remotes[name] = remote
	}
	return binding, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command(realGitBinary(), args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func handleGitBuiltinRequest(req *ExecRequest, entry *tokenEntry) *ExecResponse {
	if !entry.hasBuiltinPolicy("git") {
		return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: git"}
	}
	if entry.Git == nil {
		return &ExecResponse{ExitCode: 1, Stderr: "git builtin unavailable for this token"}
	}
	if err := validateGitBuiltinCwd(req.Cwd, entry); err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}

	// req.Git が設定済みの場合は pre-parsed path（後方互換・直接呼び出し）。
	gitReq := req.Git
	if gitReq == nil {
		invocation, err := classifyGitInvocation(req.Args)
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		// local subcommand (add/commit/diff/status 等) はワークツリーで直接実行。
		// op 単位の policy チェックは行わない（localGitSubcommands 通過で一律許可）。
		if invocation.mode == gitInvocationLocal {
			return execLocalGit(req.Cwd, req.Args)
		}
		gitReq = invocation.request
	}

	// op 制限は登録時にスタンプされた BuiltinPolicy で判定する。
	// role 判定は orchestrator.DefaultBuiltinPolicies で行われ、broker はそれを参照するのみ。
	if !entry.allowsBuiltinOp("git", string(gitReq.Op)) {
		return &ExecResponse{
			ExitCode: 1,
			Stderr:   fmt.Sprintf("git op %q not allowed by policy", gitReq.Op),
		}
	}

	if gitReq.Op == GitOpPush {
		slog.Info("git builtin push requested",
			"job_id", entry.Context.JobID,
			"task_id", entry.Context.TaskID,
			"project_id", entry.Context.ProjectID,
			"role", entry.Context.Role,
			"remote", gitReq.Remote,
			"refspecs", gitReq.Refspecs,
			"force_with_lease", gitReq.ForceWithLease,
			"worktree_root", entry.Git.WorktreeRoot,
		)
	}
	return execGitBuiltin(gitReq, entry.Git)
}

func validateGitBuiltinCwd(cwd string, entry *tokenEntry) error {
	if cwd == "" {
		return fmt.Errorf("cwd required")
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be absolute")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cwd does not exist")
		}
		return fmt.Errorf("stat cwd: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cwd must be a directory")
	}
	if entry != nil {
		if policy, ok := entry.BuiltinPolicies["git"]; ok && policy.AllowsCwd(cwd) {
			return nil
		}
		if entry.Git != nil {
			root := entry.Git.WorktreeRoot
			if cwd == root || strings.HasPrefix(cwd, root+"/") {
				return nil
			}
		}
	}
	return fmt.Errorf("git builtin is restricted to the current worktree")
}

func execLocalGit(cwd string, args []string) *ExecResponse {
	cmd := exec.Command(realGitBinary(), args...)
	cmd.Dir = cwd

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
	}
	return &ExecResponse{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
}

func execGitBuiltin(req *GitRequest, binding *GitBinding) *ExecResponse {
	if req == nil {
		return &ExecResponse{ExitCode: 1, Stderr: "missing git request"}
	}

	lock := gitRepoLock(binding.WorktreeRoot)
	lock.Lock()
	defer lock.Unlock()

	remoteName, remote, err := resolveGitRemote(req, binding)
	if err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}

	args, err := buildGitBuiltinArgs(req, binding, remoteName, remote)
	if err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}

	cmd := exec.Command(realGitBinary(), args...)
	cmd.Dir = binding.WorktreeRoot
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
	}

	if req.Op == GitOpPush {
		slog.Info("git builtin push completed",
			"worktree_root", binding.WorktreeRoot,
			"remote", remoteName,
			"refspecs", req.Refspecs,
			"exit_code", exitCode,
			"stdout", strings.TrimSpace(stdout.String()),
			"stderr", strings.TrimSpace(stderr.String()),
		)
	}

	return &ExecResponse{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
}

func resolveGitRemote(req *GitRequest, binding *GitBinding) (string, GitRemote, error) {
	if req.Remote != "" {
		remote, ok := binding.Remotes[req.Remote]
		if !ok {
			return "", GitRemote{}, fmt.Errorf("git remote %q is not allowed", req.Remote)
		}
		return req.Remote, remote, nil
	}
	if len(binding.Remotes) == 1 {
		for name, remote := range binding.Remotes {
			return name, remote, nil
		}
	}
	if binding.Upstream.Remote != "" {
		remote, ok := binding.Remotes[binding.Upstream.Remote]
		if ok {
			return binding.Upstream.Remote, remote, nil
		}
	}
	return "", GitRemote{}, fmt.Errorf("git remote must be specified explicitly")
}

func buildGitBuiltinArgs(req *GitRequest, binding *GitBinding, remoteName string, remote GitRemote) ([]string, error) {
	base := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.sshCommand=false",
		"-c", "core.attributesfile=/dev/null",
		"-c", "core.editor=false",
	}
	switch req.Op {
	case GitOpFetch:
		args := append(base, "fetch")
		if req.DryRun {
			args = append(args, "--dry-run")
		}
		if req.Verbose {
			args = append(args, "--verbose")
		}
		if req.Quiet {
			args = append(args, "--quiet")
		}
		if req.Prune {
			args = append(args, "--prune")
		}
		if req.Tags {
			args = append(args, "--tags")
		}
		if req.Force {
			args = append(args, "--force")
		}
		if remote.FetchURL == "" {
			return nil, fmt.Errorf("git remote %q has no fetch url", remoteName)
		}
		args = append(args, remote.FetchURL)
		refspecs, err := resolveGitFetchRefspecs(req, binding, remoteName)
		if err != nil {
			return nil, err
		}
		args = append(args, refspecs...)
		return args, nil
	case GitOpPush:
		args := append(base, "push")
		if req.DryRun {
			args = append(args, "--dry-run")
		}
		if req.Verbose {
			args = append(args, "--verbose")
		}
		if req.Quiet {
			args = append(args, "--quiet")
		}
		if req.Porcelain {
			args = append(args, "--porcelain")
		}
		if req.ForceWithLease {
			args = append(args, "--force-with-lease")
		}
		pushURL := remote.PushURL
		if pushURL == "" {
			pushURL = remote.FetchURL
		}
		if pushURL == "" {
			return nil, fmt.Errorf("git remote %q has no push url", remoteName)
		}
		args = append(args, pushURL)
		refspecs, err := resolveGitPushRefspecs(req, binding, remoteName)
		if err != nil {
			return nil, err
		}
		args = append(args, refspecs...)
		return args, nil
	default:
		return nil, fmt.Errorf("unsupported git operation %q", req.Op)
	}
}

func resolveGitFetchRefspecs(req *GitRequest, binding *GitBinding, remoteName string) ([]string, error) {
	if len(req.Refspecs) > 0 {
		return append([]string(nil), req.Refspecs...), nil
	}
	if binding.Upstream.Remote == remoteName && binding.Upstream.MergeRef != "" {
		branch := strings.TrimPrefix(binding.Upstream.MergeRef, "refs/heads/")
		return []string{binding.Upstream.MergeRef + ":refs/remotes/" + remoteName + "/" + branch}, nil
	}
	return nil, fmt.Errorf("git fetch without refspec requires an upstream branch")
}

func resolveGitPushRefspecs(req *GitRequest, binding *GitBinding, remoteName string) ([]string, error) {
	if len(req.Refspecs) > 0 {
		return append([]string(nil), req.Refspecs...), nil
	}

	currentBranch, err := gitOutput(binding.WorktreeRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || currentBranch == "" {
		return nil, fmt.Errorf("git push without refspec requires a checked out branch")
	}

	targetRef := "refs/heads/" + currentBranch
	if binding.SnapshotBranch == currentBranch && binding.Upstream.Remote == remoteName && binding.Upstream.MergeRef != "" {
		targetRef = binding.Upstream.MergeRef
	}
	return []string{"HEAD:" + targetRef}, nil
}

func gitRepoLock(root string) *sync.Mutex {
	lock, _ := gitRepoLocks.LoadOrStore(root, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func logGitBindingSnapshot(ctx TokenContext, binding *GitBinding, err error) {
	if err != nil {
		slog.Warn("git builtin snapshot failed", "project_id", ctx.ProjectID, "project_dir", ctx.ProjectDir, "worktree_dir", ctx.WorktreeDir, "error", err)
		return
	}
	slog.Info("git builtin snapshot ready", "project_id", ctx.ProjectID, "worktree_root", binding.WorktreeRoot, "remotes", len(binding.Remotes))
}
