package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

var realGitPath = "/usr/bin/git"

func realGitBinary() string {
	return filepath.Clean(realGitPath)
}

// Host-side git execution timeouts. These bound an otherwise-unbounded
// cmd.Run(): without them a hung git (e.g. a network op whose connect stalls)
// blocks forever, the agent's tool call eventually times out, and it sees an
// empty result with no indication of the cause. Vars (not consts) so tests can
// shorten them.
var (
	// gitDirectTimeout bounds local ops (rev-parse, status, commit, merge, …).
	gitDirectTimeout = 120 * time.Second
	// gitNetworkTimeout bounds fetch/push, which legitimately take longer.
	gitNetworkTimeout = 300 * time.Second
	// gitWaitDelay force-closes the output pipes shortly after git is killed,
	// so cmd.Run() returns even when git left children (network helpers, the
	// shell in tests) holding the pipe open. Without it Wait() blocks on those
	// grandchildren and the deadline is defeated.
	gitWaitDelay = 2 * time.Second
)

// runGitCommand runs cmd, enforcing ctx's deadline (exec.CommandContext kills
// the process when ctx fires). A deadline hit is surfaced as a non-zero exit
// with an explicit message rather than an empty/ambiguous result.
func runGitCommand(ctx context.Context, cmd *exec.Cmd, stdout, stderr *bytes.Buffer) *ExecResponse {
	cmd.WaitDelay = gitWaitDelay
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		msg := strings.TrimRight(stderr.String(), "\n")
		if msg != "" {
			msg += "\n"
		}
		msg += "boid: git timed out and was killed"
		return &ExecResponse{ExitCode: 1, Stdout: stdout.String(), Stderr: msg}
	}
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &ExecResponse{ExitCode: 1, Stdout: stdout.String(), Stderr: err.Error()}
		}
	}
	return &ExecResponse{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}
}

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
		// direct subcommand (commit/merge/rebase/log/diff/worktree 等) は
		// push/fetch のような送信先制御が不要なため op policy をスキップして
		// host の git をそのまま実行する。cwd 制限は上の validateGitBuiltinCwd
		// で既に適用済み (WorktreeRoot 外・AllowedCwdRoots 外は弾かれる)。
		if invocation.mode == gitInvocationDirect {
			return execDirectGit(req.Cwd, req.Args)
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

	if gitReq.Op == GitOpCloneLocal {
		return execGitCloneLocal(gitReq, entry)
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

func execDirectGit(cwd string, args []string) *ExecResponse {
	ctx, cancel := context.WithTimeout(context.Background(), gitDirectTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, realGitBinary(), args...)
	cmd.Dir = cwd

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	return runGitCommand(ctx, cmd, &stdout, &stderr)
}

func execGitBuiltin(req *GitRequest, binding *GitBinding) *ExecResponse {
	if req == nil {
		return &ExecResponse{ExitCode: 1, Stderr: "missing git request"}
	}

	// No broker-side lock here. git's own ref/index locking already makes
	// concurrent operations on the same worktree safe — they fail fast with a
	// clear error, they do not corrupt. A mutex held across the git run added
	// nothing git wasn't already doing, while turning a single hung git into a
	// permanent deadlock of every later op on the worktree. We rely on git.
	remoteName, remote, err := resolveGitRemote(req, binding)
	if err != nil {
		// The binding's remotes are snapshotted once, at token registration. A
		// remote configured later in the same session — e.g. `gh repo create`,
		// which adds `origin` to a freshly `git init`-ed worktree after the
		// token already exists — is absent from that snapshot, so resolution
		// fails even though the remote now exists on disk. Re-read the worktree
		// once and retry against the fresh binding.
		//
		// This does not weaken the trusted-snapshot guarantee that
		// TestBroker_GitBuiltinPushUsesTrustedSnapshot pins down: an
		// already-known remote resolves from the snapshot above (no re-capture),
		// so its URL stays fixed and cannot be redirected via `remote set-url`.
		// We only re-read when resolution found no remote to pin in the first
		// place. The fresh binding is used locally for this op only; the cached
		// entry.Git is left untouched to avoid racing concurrent ops.
		if fresh, recErr := captureGitBinding(binding.ProjectDir, binding.WorktreeRoot); recErr == nil {
			if rn, r, rerr := resolveGitRemote(req, fresh); rerr == nil {
				binding, remoteName, remote, err = fresh, rn, r, nil
			}
		}
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
	}

	args, err := buildGitBuiltinArgs(req, binding, remoteName, remote)
	if err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitNetworkTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, realGitBinary(), args...)
	cmd.Dir = binding.WorktreeRoot
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	resp := runGitCommand(ctx, cmd, &stdout, &stderr)

	if req.Op == GitOpPush {
		slog.Info("git builtin push completed",
			"worktree_root", binding.WorktreeRoot,
			"remote", remoteName,
			"refspecs", req.Refspecs,
			"exit_code", resp.ExitCode,
			"stdout", strings.TrimSpace(stdout.String()),
			"stderr", strings.TrimSpace(stderr.String()),
		)
		if req.SetUpstream && !req.DryRun && resp.ExitCode == 0 {
			if err := setGitUpstreamConfig(binding.WorktreeRoot, remoteName, req, binding); err != nil {
				slog.Warn("git push --set-upstream: could not configure upstream", "err", err)
				resp.Stderr += "\nwarning: could not set upstream tracking: " + err.Error()
			}
		}
	}

	return resp
}

// setGitUpstreamConfig sets branch.<current>.remote and branch.<current>.merge
// after a successful push with -u / --set-upstream. git cannot set upstream when
// pushing to a URL directly (instead of a named remote), so we apply the config
// ourselves using the named remote we already resolved from the binding.
func setGitUpstreamConfig(worktreeRoot, remoteName string, req *GitRequest, binding *GitBinding) error {
	currentBranch, err := gitOutput(worktreeRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || currentBranch == "" {
		return fmt.Errorf("could not determine current branch")
	}

	refspecs, err := resolveGitPushRefspecs(req, binding, remoteName)
	if err != nil {
		return fmt.Errorf("could not resolve push refspecs: %w", err)
	}

	mergeRef := upstreamMergeRef(refspecs, currentBranch)

	if _, err := gitOutput(worktreeRoot, "config", "branch."+currentBranch+".remote", remoteName); err != nil {
		return fmt.Errorf("set branch remote: %w", err)
	}
	if _, err := gitOutput(worktreeRoot, "config", "branch."+currentBranch+".merge", mergeRef); err != nil {
		return fmt.Errorf("set branch merge: %w", err)
	}
	return nil
}

// upstreamMergeRef extracts the remote-side ref from the push refspecs.
// For a refspec like "HEAD:refs/heads/feature" it returns "refs/heads/feature".
// If no target is found it falls back to "refs/heads/<currentBranch>".
func upstreamMergeRef(refspecs []string, currentBranch string) string {
	for _, refspec := range refspecs {
		if idx := strings.Index(refspec, ":"); idx >= 0 {
			target := refspec[idx+1:]
			if target != "" {
				if !strings.HasPrefix(target, "refs/") {
					target = "refs/heads/" + target
				}
				return target
			}
		}
	}
	return "refs/heads/" + currentBranch
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
	case GitOpPushDelete:
		args := append(base, "push", "--delete")
		if req.DryRun {
			args = append(args, "--dry-run")
		}
		if req.Verbose {
			args = append(args, "--verbose")
		}
		if req.Quiet {
			args = append(args, "--quiet")
		}
		pushURL := remote.PushURL
		if pushURL == "" {
			pushURL = remote.FetchURL
		}
		if pushURL == "" {
			return nil, fmt.Errorf("git remote %q has no push url", remoteName)
		}
		if len(req.Refspecs) == 0 {
			return nil, fmt.Errorf("git push --delete requires at least one branch or refspec")
		}
		args = append(args, pushURL)
		for _, refspec := range req.Refspecs {
			// :refs/heads/branch 形式の場合はコロンを除去してブランチ名だけ渡す
			branch := strings.TrimPrefix(refspec, ":")
			args = append(args, branch)
		}
		return args, nil
	default:
		return nil, fmt.Errorf("unsupported git operation %q", req.Op)
	}
}

func resolveGitFetchRefspecs(req *GitRequest, binding *GitBinding, remoteName string) ([]string, error) {
	if len(req.Refspecs) > 0 {
		expanded := make([]string, len(req.Refspecs))
		for i, refspec := range req.Refspecs {
			expanded[i] = expandFetchRefspec(refspec, remoteName)
		}
		return expanded, nil
	}
	if binding.Upstream.Remote == remoteName && binding.Upstream.MergeRef != "" {
		branch := strings.TrimPrefix(binding.Upstream.MergeRef, "refs/heads/")
		return []string{binding.Upstream.MergeRef + ":refs/remotes/" + remoteName + "/" + branch}, nil
	}
	return nil, fmt.Errorf("git fetch without refspec requires an upstream branch")
}

// expandFetchRefspec expands a bare branch name to a full refspec that updates
// the remote tracking branch alongside FETCH_HEAD. Without expansion, running
// "git fetch <url> main" only writes FETCH_HEAD and leaves refs/remotes/origin/main
// stale, causing git rev-parse origin/main to return an old SHA that differs from
// git ls-remote origin main.
//
// Refspecs that already contain ':' are returned unchanged. Tags and special refs
// (HEAD, refs/tags/*, refs/notes/*) are also returned unchanged since they do not
// map to remote tracking branches.
func expandFetchRefspec(refspec, remoteName string) string {
	force := strings.HasPrefix(refspec, "+")
	bare := refspec
	if force {
		bare = refspec[1:]
	}

	// Already has an explicit destination; don't modify it.
	if strings.Contains(bare, ":") {
		return refspec
	}
	// Special refs that don't map to remote tracking branches.
	if bare == "HEAD" || strings.HasPrefix(bare, "refs/tags/") || strings.HasPrefix(bare, "refs/notes/") {
		return refspec
	}

	var src, dst string
	if strings.HasPrefix(bare, "refs/heads/") {
		branch := strings.TrimPrefix(bare, "refs/heads/")
		src = bare
		dst = "refs/remotes/" + remoteName + "/" + branch
	} else if strings.HasPrefix(bare, "refs/") {
		// Other full ref paths: pass through unchanged.
		return refspec
	} else {
		// Bare branch name (e.g. "main") — treat as refs/heads/<name>.
		src = "refs/heads/" + bare
		dst = "refs/remotes/" + remoteName + "/" + bare
	}

	result := src + ":" + dst
	if force {
		return "+" + result
	}
	return result
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


// execGitCloneLocal handles the GitOpCloneLocal operation: clone a peer project
// directory (source) into a subdirectory of the current worktree (dest).
// Both paths are validated against the token's WorkspacePeers and WorktreeRoot
// to prevent sandbox escapes and path traversal.
func execGitCloneLocal(req *GitRequest, entry *tokenEntry) *ExecResponse {
	if req.Source == "" {
		return &ExecResponse{ExitCode: 1, Stderr: "git clone --local: source path is required"}
	}

	worktreeRoot := entry.Git.WorktreeRoot
	peers := entry.Context.WorkspacePeers

	// Canonicalize paths to prevent traversal via ".." components.
	cleanSource := filepath.Clean(req.Source)
	cleanDest := filepath.Clean(req.Dest)

	// Validate source is within a known peer project directory.
	validSource := false
	for _, peerPath := range peers {
		if peerPath == "" {
			continue
		}
		cleanPeer := filepath.Clean(peerPath)
		if cleanSource == cleanPeer || strings.HasPrefix(cleanSource, cleanPeer+"/") {
			validSource = true
			break
		}
	}
	if !validSource {
		return &ExecResponse{
			ExitCode: 1,
			Stderr:   fmt.Sprintf("git clone --local: source %q is not within a workspace peer project", req.Source),
		}
	}

	// Validate dest is within the worktree root.
	if worktreeRoot == "" {
		return &ExecResponse{ExitCode: 1, Stderr: "git clone --local: worktree root not configured"}
	}
	cleanWorktree := filepath.Clean(worktreeRoot)
	if cleanDest != cleanWorktree && !strings.HasPrefix(cleanDest, cleanWorktree+"/") {
		return &ExecResponse{
			ExitCode: 1,
			Stderr:   fmt.Sprintf("git clone --local: dest %q is not within the current worktree", req.Dest),
		}
	}

	// Build the git command. The broker re-constructs args from the validated
	// paths, ignoring what the sandbox originally sent.
	base := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.attributesfile=/dev/null",
		"-c", "core.editor=false",
	}
	args := append(base, "clone", "--local", "--", cleanSource, cleanDest)

	ctx, cancel := context.WithTimeout(context.Background(), gitDirectTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, realGitBinary(), args...)
	cmd.Dir = worktreeRoot
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	return runGitCommand(ctx, cmd, &stdout, &stderr)
}

func logGitBindingSnapshot(ctx TokenContext, binding *GitBinding, err error) {
	if err != nil {
		slog.Warn("git builtin snapshot failed", "project_id", ctx.ProjectID, "project_dir", ctx.ProjectDir, "worktree_dir", ctx.WorktreeDir, "error", err)
		return
	}
	slog.Info("git builtin snapshot ready", "project_id", ctx.ProjectID, "worktree_root", binding.WorktreeRoot, "remotes", len(binding.Remotes))
}
