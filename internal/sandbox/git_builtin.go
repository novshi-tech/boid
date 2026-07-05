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
	gitReq := invocation.request

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

// ---------- argv classification (handleGitBuiltinRequest の唯一の呼び出し元) ----------

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
// clone は --local 限定の brokered op として別途許可するため deny-list から除外。
var deniedGitSubcommands = map[string]struct{}{
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
	case "clone":
		req, err := parseGitCloneRequest(rest)
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

// parseGitCloneRequest parses "git clone" arguments and produces a GitRequest
// for GitOpCloneLocal. Only --local clones are permitted; network clones and
// dangerous flags are rejected. The broker re-validates source and dest.
func parseGitCloneRequest(args []string) (*GitRequest, error) {
	hasLocal := false
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "--local" || arg == "-l" {
			hasLocal = true
			continue
		}
		// Dangerous flags that could cause code execution or repo pollution.
		if isDeniedCloneFlag(arg) {
			return nil, fmt.Errorf("git clone option %q is not allowed", arg)
		}
		// Permitted passthrough flags (depth, branch, no-tags, etc.).
		if strings.HasPrefix(arg, "-") && arg != "-" {
			// Allow only explicitly whitelisted flags; reject anything else.
			if !isAllowedCloneFlag(arg) {
				return nil, fmt.Errorf("git clone option %q is not allowed", arg)
			}
			// Consume value for flags that take an argument (e.g. --branch value).
			if cloneFlagTakesValue(arg) && !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		positional = append(positional, args[i:]...)
		break
	}

	if !hasLocal {
		return nil, fmt.Errorf("git clone requires --local flag (network clones are not allowed)")
	}
	if len(positional) == 0 {
		return nil, fmt.Errorf("git clone requires a source path")
	}
	source := positional[0]
	dest := ""
	if len(positional) >= 2 {
		dest = positional[1]
	}
	return &GitRequest{Op: GitOpCloneLocal, Source: source, Dest: dest}, nil
}

// isDeniedCloneFlag reports whether a clone flag is dangerous and must be rejected.
func isDeniedCloneFlag(arg string) bool {
	// Flags that inject config, hooks, alternate object stores, or external commands.
	dangerousPrefixes := []string{
		"-c", "--upload-pack", "--template", "--config",
		"--reference", "--separate-git-dir", "--recurse-submodules",
		"--server-option",
	}
	for _, prefix := range dangerousPrefixes {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") || strings.HasPrefix(arg, prefix+" ") {
			return true
		}
	}
	// Short forms
	if arg == "-u" { // --upload-pack short form
		return true
	}
	return false
}

// isAllowedCloneFlag reports whether a clone flag is safe to pass through.
func isAllowedCloneFlag(arg string) bool {
	allowed := []string{
		"--no-hardlinks", "--shared", "--no-checkout", "-n",
		"--bare", "--mirror", "--origin", "-o",
		"--branch", "-b", "--depth", "--shallow-since",
		"--shallow-exclude", "--single-branch", "--no-single-branch",
		"--no-tags", "--sparse", "--filter", "--also-filter-submodules",
		"--quiet", "-q", "--verbose", "-v", "--progress", "--no-progress",
		"--bundle-uri",
	}
	for _, a := range allowed {
		if arg == a || strings.HasPrefix(arg, a+"=") {
			return true
		}
	}
	return false
}

// cloneFlagTakesValue reports whether a clone flag consumes the next argument.
func cloneFlagTakesValue(arg string) bool {
	valueFlags := []string{"--origin", "-o", "--branch", "-b", "--depth",
		"--shallow-since", "--shallow-exclude", "--filter"}
	for _, f := range valueFlags {
		if arg == f {
			return true
		}
	}
	return false
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
			case "-u", "--set-upstream":
				req.SetUpstream = true
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
