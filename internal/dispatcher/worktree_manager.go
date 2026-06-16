package dispatcher

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// WorktreeManager handles git worktree lifecycle for task isolation.
type WorktreeManager struct {
	RootDir string // e.g. ~/.local/share/boid/worktrees
	DB      *sql.DB
	GitBin  string // path to git binary; defaults to "git"
}

func (m *WorktreeManager) gitBin() string {
	if m.GitBin != "" {
		return m.GitBin
	}
	return "git"
}

// resolveBaseBranch resolves a local branch name to its remote tracking counterpart.
// baseBranch must be non-empty — P1 guarantees the service layer expands
// ${current_branch} before reaching the dispatcher.
// If origin/<baseBranch> exists, returns ("origin/<baseBranch>", true, nil).
// If only a local branch exists, returns (baseBranch, false, nil).
// If neither origin/<baseBranch> nor the local branch exists, returns an error.
func (m *WorktreeManager) resolveBaseBranch(projectDir, baseBranch string) (string, bool, error) {
	if baseBranch == "" {
		return "", false, fmt.Errorf("resolveBaseBranch: baseBranch must be non-empty")
	}
	if strings.HasPrefix(baseBranch, "origin/") {
		return baseBranch, true, nil
	}
	cmd := exec.Command(m.gitBin(), "rev-parse", "--verify", "origin/"+baseBranch)
	cmd.Dir = projectDir
	if err := cmd.Run(); err == nil {
		return "origin/" + baseBranch, true, nil
	}
	// origin/<base> not found; check if a local branch exists.
	localCheck := exec.Command(m.gitBin(), "rev-parse", "--verify", baseBranch)
	localCheck.Dir = projectDir
	if localCheck.Run() == nil {
		return baseBranch, false, nil
	}
	return "", false, fmt.Errorf("base branch %q not found locally or on origin", baseBranch)
}

// ensureBaseBranchExists is the case-3 mitigation: if the requested base
// branch does not exist locally or on origin, create a local branch with
// that name from a stable fork point.
//
// The fork point is resolved in this order:
//  1. forkPoint argument (from ProjectMeta.ForkPoint), if non-empty and
//     `git rev-parse --verify` recognises it
//  2. refs/remotes/origin/HEAD, if `git symbolic-ref` can resolve it
//     (i.e. `git remote set-head origin --auto` has been run on the clone)
//
// The project root's working-tree HEAD is intentionally never used: it can
// drift to an unexpected branch between task creation and dispatch, which
// historically produced new branches forked from the wrong commit.
//
// No-op when:
//   - baseBranch carries an explicit "origin/" prefix (callers asking for a
//     remote-tracking ref accept that it must exist; auto-creating a local
//     branch would silently subvert that intent)
//   - the local branch already exists
//   - origin/<baseBranch> exists
//
// Returns an error when:
//   - baseBranch is empty (P1: must be resolved by service layer)
//   - no usable fork point can be resolved
//   - the git branch command itself fails
func (m *WorktreeManager) ensureBaseBranchExists(projectDir, baseBranch, forkPoint string) error {
	if baseBranch == "" {
		return fmt.Errorf("ensureBaseBranchExists: baseBranch must be non-empty")
	}
	base := baseBranch
	if strings.HasPrefix(base, "origin/") {
		return nil
	}
	// Already exists locally?
	if exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", "--quiet", base).Run() == nil {
		return nil
	}
	// Already exists on origin? Do not fetch — the existing remote ref view
	// is all we trust at this point (parity with ClassifyBaseBranch).
	if exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", "--quiet", "origin/"+base).Run() == nil {
		return nil
	}
	// Case 3: derive the branch from a stable fork point. The project root's
	// working-tree HEAD is intentionally not consulted.
	start, source, err := m.resolveCase3ForkStart(projectDir, forkPoint)
	if err != nil {
		return fmt.Errorf("cannot create base branch %q in %s: %w", base, projectDir, err)
	}
	cmd := exec.Command(m.gitBin(), "-C", projectDir, "branch", base, start)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch %s %s: %w\n%s", base, start, err, strings.TrimSpace(string(out)))
	}
	slog.Info("base branch created (case 3)",
		"project_dir", projectDir, "base_branch", base, "fork_start", start, "fork_source", source)
	return nil
}

// resolveCase3ForkStart picks the start point for a case-3 base-branch
// creation. forkPoint (from project.yaml) takes precedence; otherwise
// origin/HEAD is consulted. Returns (ref, source, error) where source is a
// short label suitable for logging.
//
// When the chosen start references an origin remote-tracking ref, a
// `git fetch origin <branch>` is issued first so we fork from the latest
// upstream commit rather than whatever was in the local cache. Mirrors the
// case-2 fetch in Create. Fetch failures degrade to a warning + the existing
// local ref view (parity with case 2); a missing ref afterwards still errors.
func (m *WorktreeManager) resolveCase3ForkStart(projectDir, forkPoint string) (string, string, error) {
	if forkPoint != "" {
		if strings.HasPrefix(forkPoint, "origin/") {
			m.fetchOriginBranch(projectDir, strings.TrimPrefix(forkPoint, "origin/"), "fork_point")
		}
		if err := exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", "--quiet", forkPoint).Run(); err != nil {
			return "", "", fmt.Errorf("fork_point %q does not resolve in %s (configure project.yaml fork_point or fetch the ref)", forkPoint, projectDir)
		}
		return forkPoint, "project.yaml fork_point", nil
	}
	// Fall back to origin/HEAD. symbolic-ref returns the resolved ref name
	// (e.g. "refs/remotes/origin/main"); we strip the prefix to get the
	// upstream branch name and fetch it so the resulting fork is fresh.
	out, err := exec.Command(m.gitBin(), "-C", projectDir, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return "", "", fmt.Errorf("no fork point available: project.yaml fork_point is unset and refs/remotes/origin/HEAD is not configured (run `git remote set-head origin --auto` in %s or set fork_point in project.yaml)", projectDir)
	}
	headRef := strings.TrimSpace(string(out))
	if remoteBranch := strings.TrimPrefix(headRef, "refs/remotes/origin/"); remoteBranch != headRef && remoteBranch != "" {
		m.fetchOriginBranch(projectDir, remoteBranch, "origin/HEAD")
	}
	return "refs/remotes/origin/HEAD", "origin/HEAD", nil
}

// fetchOriginBranch issues `git fetch origin <branch>` and logs a warning
// on failure without surfacing the error. The caller still re-verifies the
// resulting ref, so a stale cache is acceptable but a missing ref will be
// rejected downstream.
func (m *WorktreeManager) fetchOriginBranch(projectDir, branch, reason string) {
	cmd := exec.Command(m.gitBin(), "-C", projectDir, "fetch", "origin", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("case 3 pre-fork fetch failed; falling back to local remote-tracking ref",
			"project_dir", projectDir, "branch", branch, "reason", reason,
			"error", err, "output", strings.TrimSpace(string(out)))
	}
}

// EnforceHeadOnBaseBranch is the Phase 2-2 case 1 HEAD guard. Supervisors
// running in the project dir (worktree=false) require the project HEAD to
// remain on the resolved baseBranch from task creation time through job
// dispatch. A mismatch means the user (or another process) has moved the
// project branch while a supervisor task was queued; running the supervisor
// against an unexpected branch silently is the foot-gun that this guard
// rejects.
//
// Returns nil when baseBranch is empty (no expectation to enforce), nil on a
// successful match, and an error otherwise. Detached HEAD is reported as an
// error: a case 1 task should never have been classified for a detached
// project at creation time, so this code path indicates a state divergence
// that must abort the run.
func (m *WorktreeManager) EnforceHeadOnBaseBranch(projectDir, baseBranch string) error {
	if baseBranch == "" {
		return nil
	}
	// "origin/main" and "main" both expect HEAD on "main".
	expected := strings.TrimPrefix(baseBranch, "origin/")

	cmd := exec.Command(m.gitBin(), "-C", projectDir, "symbolic-ref", "--quiet", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return fmt.Errorf("project HEAD guard: %s is in detached HEAD state (expected branch %q)", projectDir, expected)
		}
		return fmt.Errorf("project HEAD guard: git symbolic-ref failed in %s: %w", projectDir, err)
	}
	actual := strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/heads/")
	if actual != expected {
		return fmt.Errorf("project HEAD guard: %s is on %q but task base_branch is %q (refusing to run supervisor against unexpected branch)",
			projectDir, actual, expected)
	}
	return nil
}

// resolveRecreateBasePoint picks the start-point for a fresh branch when
// Recreate cannot find either the local or remote task branch. Prefers
// origin/<base> (already fetched by the caller), falls back to the local base.
func (m *WorktreeManager) resolveRecreateBasePoint(projectDir, recordedBase string) (string, error) {
	base := strings.TrimPrefix(recordedBase, "origin/")
	if base == "" {
		base = "main"
	}
	for _, candidate := range []string{"origin/" + base, base} {
		cmd := exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", candidate)
		if cmd.Run() == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("base branch %q not found (checked origin/%s and %s)", base, base, base)
}

// branchPrefix is the hardcoded prefix used for all per-task worktree
// branches. Phase 3-1 removed the per-behavior `branch_prefix` knob; the
// constant is centralised here so that other code paths reach for the same
// literal.
const branchPrefix = "boid/"

// resolveForkPoint resolves the git start-point for a child task's new
// boid/<id8> branch when CreateOpts.ForkPoint is explicitly set (P3).
//
// If forkPoint starts with "boid/", it is a local-only branch (the parent's
// worktree HEAD). Only a local existence check is performed — no remote fetch
// is attempted, because local-only branches are never pushed.
//
// For any other prefix, the same resolution logic as resolveBaseBranch is
// applied (origin/<name> if available, remote fetch, local fallback).
func (m *WorktreeManager) resolveForkPoint(projectDir, forkPoint string) (string, error) {
	if strings.HasPrefix(forkPoint, branchPrefix) {
		// Local-only branch: verify it exists and return as-is.
		if err := exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", "--quiet", forkPoint).Run(); err != nil {
			return "", fmt.Errorf("fork point %q not found locally (parent task worktree missing?): %w", forkPoint, err)
		}
		return forkPoint, nil
	}
	// Remote-backed fork point: resolve the same way as baseBranch.
	resolved, shouldFetch, err := m.resolveBaseBranch(projectDir, forkPoint)
	if err != nil {
		return "", fmt.Errorf("resolve fork point %q: %w", forkPoint, err)
	}
	if shouldFetch {
		branchToFetch := strings.TrimPrefix(resolved, "origin/")
		fetchCmd := exec.Command(m.gitBin(), "fetch", "origin", branchToFetch)
		fetchCmd.Dir = projectDir
		if out, fetchErr := fetchCmd.CombinedOutput(); fetchErr != nil {
			localCheck := exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", branchToFetch)
			if localCheck.Run() != nil {
				return "", fmt.Errorf("git fetch origin %s for fork point failed and local branch not found: %w\n%s",
					branchToFetch, fetchErr, strings.TrimSpace(string(out)))
			}
			slog.Warn("git fetch for fork point failed, falling back to local branch",
				"fork_point", forkPoint, "error", fetchErr)
			resolved = branchToFetch
		}
	}
	return resolved, nil
}

// CreateOpts controls optional worktree creation behaviour.
type CreateOpts struct {
	// CheckoutBranch, when non-empty, causes Create to check out an existing
	// branch directly (git worktree add <path> <branch>) rather than creating
	// a new boid/<id8> branch. Used for root tasks (ParentID == "") so the
	// worktree HEAD matches task.BaseBranch directly (P2).
	CheckoutBranch string

	// ForkPoint overrides the start-point used when creating a new boid/<id8>
	// branch (CheckoutBranch == ""). Defaults to baseBranch when empty.
	// Reserved for P3 (child fork from parent HEAD branch); unused in P2.
	ForkPoint string

	// BaseBranchForkPoint is the start point used when the requested
	// baseBranch does not exist yet (ClassifyBaseBranch case 3) and must
	// be created locally. Sourced from ProjectMeta.ForkPoint via
	// Visibility.ForkPoint. Empty falls back to refs/remotes/origin/HEAD;
	// if that is also unset, Create returns an error.
	BaseBranchForkPoint string
}

func (m *WorktreeManager) Create(projectDir, projectID, taskID, baseBranch string, opts CreateOpts) (*Worktree, error) {
	shortID := taskID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	var branch string
	if opts.CheckoutBranch != "" {
		// Root task: occupy the base_branch directly (P2).
		branch = opts.CheckoutBranch
	} else {
		branch = branchPrefix + shortID
	}
	wtPath := filepath.Join(m.RootDir, projectID, shortID)

	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktree parent: %w", err)
	}

	// Case 3: if baseBranch is unknown to both local and origin we create it
	// from a stable fork point (project.yaml fork_point or origin/HEAD —
	// never the project root's working-tree HEAD) before falling through
	// to the normal resolveBaseBranch path. Worktree creation for case 1 /
	// case 2 is unaffected (the function returns early with no-op when the
	// branch already exists).
	if err := m.ensureBaseBranchExists(projectDir, baseBranch, opts.BaseBranchForkPoint); err != nil {
		return nil, err
	}

	resolvedBase, shouldFetch, err := m.resolveBaseBranch(projectDir, baseBranch)
	if err != nil {
		return nil, err
	}

	if shouldFetch {
		branchToFetch := strings.TrimPrefix(resolvedBase, "origin/")
		fetchCmd := exec.Command(m.gitBin(), "fetch", "origin", branchToFetch)
		fetchCmd.Dir = projectDir
		if out, fetchErr := fetchCmd.CombinedOutput(); fetchErr != nil {
			// fetch failed; fall back to local branch only if it actually exists.
			localCheck := exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", branchToFetch)
			if localCheck.Run() != nil {
				return nil, fmt.Errorf("git fetch origin %s failed and local branch not found: %w\n%s",
					branchToFetch, fetchErr, strings.TrimSpace(string(out)))
			}
			slog.Warn("git fetch failed, falling back to local branch",
				"branch", branchToFetch, "error", fetchErr, "output", strings.TrimSpace(string(out)))
			resolvedBase = branchToFetch
		}
	}

	var cmd *exec.Cmd
	if opts.CheckoutBranch != "" {
		// Root task: check out the existing base branch directly. No new branch
		// is created; the worktree HEAD is set to opts.CheckoutBranch (P2).
		cmd = exec.Command(m.gitBin(), "worktree", "add", wtPath, opts.CheckoutBranch)
	} else {
		// Child task: fork from opts.ForkPoint when set (P3), otherwise fall
		// back to the resolved baseBranch (existing pre-P3 behaviour).
		forkStart := resolvedBase
		if opts.ForkPoint != "" {
			var forkErr error
			forkStart, forkErr = m.resolveForkPoint(projectDir, opts.ForkPoint)
			if forkErr != nil {
				return nil, forkErr
			}
		}
		cmd = exec.Command(m.gitBin(), "worktree", "add", "--no-track", "-b", branch, wtPath, forkStart)
	}
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git worktree add: %w\n%s", err, out)
	}

	// Verify that HEAD points to the expected branch (DWIM guard).
	headCmd := exec.Command(m.gitBin(), "-C", wtPath, "symbolic-ref", "HEAD")
	headOut, headErr := headCmd.CombinedOutput()
	if headErr != nil {
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", wtPath).Run()
		return nil, fmt.Errorf("worktree HEAD check failed: %w", headErr)
	}
	expectedRef := "refs/heads/" + branch
	if actualRef := strings.TrimSpace(string(headOut)); actualRef != expectedRef {
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", wtPath).Run()
		return nil, fmt.Errorf("worktree HEAD mismatch: expected %s, got %s", expectedRef, actualRef)
	}

	// Ensure <wtPath>/.boid exists so the sandbox bind-mount target is present
	// even when the project's .boid is untracked (otherwise readonly worktree
	// mounts fail with EROFS during the bind setup).
	if err := os.MkdirAll(filepath.Join(wtPath, ".boid"), 0o755); err != nil {
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", wtPath).Run()
		return nil, fmt.Errorf("ensure .boid dir: %w", err)
	}

	w := &Worktree{
		TaskID:     taskID,
		ProjectID:  projectID,
		Path:       wtPath,
		Branch:     branch,
		BaseBranch: resolvedBase,
	}
	if err := CreateWorktree(m.DB, w); err != nil {
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", wtPath).Run()
		return nil, fmt.Errorf("record worktree: %w", err)
	}

	slog.Info("worktree created", "task_id", taskID, "path", wtPath, "branch", branch)
	return w, nil
}

func (m *WorktreeManager) Remove(projectDir, taskID string, deleteBranch bool) error {
	w, err := GetWorktreeByTask(m.DB, taskID)
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}
	if w == nil || w.CleanedAt != nil {
		return nil
	}

	cmd := exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", w.Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("git worktree remove failed, attempting manual cleanup",
			"error", err, "output", strings.TrimSpace(string(out)))
		os.RemoveAll(w.Path)
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "prune").Run()
	}

	// Only delete boid/* branches. Non-boid branches (e.g. base_branch of a
	// root task) are user-owned and must never be auto-deleted on cleanup.
	if deleteBranch && strings.HasPrefix(w.Branch, branchPrefix) {
		cmd := exec.Command(m.gitBin(), "-C", projectDir, "branch", "-D", w.Branch)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("git branch -D failed", "branch", w.Branch,
				"error", err, "output", strings.TrimSpace(string(out)))
		}
	}

	if err := MarkWorktreeCleaned(m.DB, taskID); err != nil {
		return fmt.Errorf("mark cleaned: %w", err)
	}

	slog.Info("worktree removed", "task_id", taskID, "path", w.Path, "branch_deleted", deleteBranch)
	return nil
}

func (m *WorktreeManager) Get(taskID string) (*Worktree, error) {
	return GetWorktreeByTask(m.DB, taskID)
}

// CleanupForTask removes the worktree filesystem for a task that has reached a
// terminal state (done / aborted). The associated boid/<id8> branch is NOT
// deleted here: that responsibility is deferred to the parent supervisor's own
// finalizeTerminal (via SweepChildBranches), so the supervisor can merge the
// child branch into the base branch in its post-execution phase before the
// branch is dropped. Root tasks (no parent) run on the base branch directly
// and have no boid/* branch to delete in the first place, so this method is
// uniformly safe regardless of whether the task has a parent.
func (m *WorktreeManager) CleanupForTask(taskID, projectDir, newStatus string) error {
	if newStatus != "done" && newStatus != "aborted" {
		return nil
	}

	w, err := m.Get(taskID)
	if err != nil {
		return err
	}
	if w == nil || w.CleanedAt != nil {
		return nil
	}

	return m.Remove(projectDir, taskID, false)
}

// SweepChildBranches deletes the boid/<id8> branches associated with the given
// task IDs. Called after a parent supervisor reaches a terminal state: its
// children's branches were retained through CleanupForTask so the supervisor
// could merge them into the base branch; once the supervisor is terminal, the
// merged refs are safe to drop.
//
// Behaviour:
//   - Task IDs with no worktree record are silently skipped (e.g. root tasks
//     that ran in the project dir directly).
//   - Non-boid/* branches are skipped (root-task base branches must never be
//     auto-deleted; the prefix check is a defence-in-depth).
//   - `git branch -D` failures are logged but not returned: a child branch may
//     have been deleted already (e.g. by an explicit user merge that ran
//     `git branch -d`), in which case the sweep is a no-op.
func (m *WorktreeManager) SweepChildBranches(projectDir string, taskIDs []string) error {
	for _, taskID := range taskIDs {
		w, err := m.Get(taskID)
		if err != nil {
			slog.Warn("sweep child branches: worktree lookup failed",
				"task_id", taskID, "error", err)
			continue
		}
		if w == nil {
			continue
		}
		if !strings.HasPrefix(w.Branch, branchPrefix) {
			continue
		}
		cmd := exec.Command(m.gitBin(), "-C", projectDir, "branch", "-D", w.Branch)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Debug("sweep child branches: git branch -D skipped",
				"task_id", taskID, "branch", w.Branch,
				"error", err, "output", strings.TrimSpace(string(out)))
			continue
		}
		slog.Info("child branch swept", "task_id", taskID, "branch", w.Branch)
	}
	return nil
}

// Recreate reconstructs a previously cleaned worktree by fetching from the remote branch.
// It reads the existing DB record (even if cleaned_at is set), fetches the remote branch,
// creates a new worktree, and clears the cleaned_at timestamp.
func (m *WorktreeManager) Recreate(projectDir string, taskID string) (*Worktree, error) {
	w, err := GetWorktreeByTask(m.DB, taskID)
	if err != nil {
		return nil, fmt.Errorf("get worktree: %w", err)
	}
	if w == nil {
		return nil, fmt.Errorf("no worktree record found for task %s", taskID)
	}

	// Fetch the remote branch so origin/<branch> is up-to-date.
	// Failure is non-fatal: fall through to the local branch check below (mirrors Create's behaviour).
	fetchCmd := exec.Command(m.gitBin(), "-C", projectDir, "fetch", "origin", w.Branch)
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		slog.Warn("git fetch failed, falling back to local branch",
			"branch", w.Branch, "error", err, "output", strings.TrimSpace(string(out)))
	}

	// Also fetch the base branch so origin/<baseBranch> is up-to-date.
	// This ensures that git merge origin/main in reworking state merges against the latest main.
	// Failure is non-fatal: log a warning and continue (e.g. local-only projects without a remote base).
	baseBranch := strings.TrimPrefix(w.BaseBranch, "origin/")
	if baseBranch == "" {
		baseBranch = "main"
	}
	fetchBaseCmd := exec.Command(m.gitBin(), "-C", projectDir, "fetch", "origin", baseBranch)
	if out, err := fetchBaseCmd.CombinedOutput(); err != nil {
		slog.Warn("git fetch base branch failed, continuing",
			"base_branch", baseBranch, "error", err, "output", strings.TrimSpace(string(out)))
	}

	if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktree parent: %w", err)
	}

	// Check whether the local branch still exists (it may have been deleted by CleanupForTask).
	localBranchCheck := exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", w.Branch)
	var wtCmd *exec.Cmd
	if localBranchCheck.Run() == nil {
		// Local branch exists; check it out directly.
		wtCmd = exec.Command(m.gitBin(), "worktree", "add", w.Path, w.Branch)
		wtCmd.Dir = projectDir
	} else if remoteCheck := exec.Command(m.gitBin(), "-C", projectDir, "rev-parse", "--verify", "origin/"+w.Branch); remoteCheck.Run() == nil {
		// Local branch was deleted but remote branch still exists; recreate local branch from remote.
		// This covers rework cycles where the branch was pushed but later pruned locally.
		wtCmd = exec.Command(m.gitBin(), "worktree", "add", "-B", w.Branch, w.Path, "origin/"+w.Branch)
		wtCmd.Dir = projectDir
	} else {
		// Neither local nor remote branch exists. This happens on rerun after abort
		// (CleanupForTask drops the local branch, and if the branch was never pushed
		// the remote has nothing either). rerun semantically means "reset and retry",
		// so we re-branch from the recorded base branch instead of failing.
		startPoint, err := m.resolveRecreateBasePoint(projectDir, w.BaseBranch)
		if err != nil {
			return nil, fmt.Errorf("local branch %q not found and cannot resolve base branch: %w", w.Branch, err)
		}
		wtCmd = exec.Command(m.gitBin(), "worktree", "add", "-b", w.Branch, w.Path, startPoint)
		wtCmd.Dir = projectDir
	}
	if out, err := wtCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	// Same rationale as Create: bind-mount target must exist on a readonly worktree.
	if err := os.MkdirAll(filepath.Join(w.Path, ".boid"), 0o755); err != nil {
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", w.Path).Run()
		return nil, fmt.Errorf("ensure .boid dir: %w", err)
	}

	if err := ClearWorktreeCleaned(m.DB, taskID); err != nil {
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", w.Path).Run()
		return nil, fmt.Errorf("clear worktree cleaned: %w", err)
	}

	w.CleanedAt = nil
	slog.Info("worktree recreated", "task_id", taskID, "path", w.Path, "branch", w.Branch)
	return w, nil
}

// EnsureBindingTargets pre-creates additional_bindings mount targets that fall
// under worktreePath, so a subsequent readonly bind-remount of the worktree
// does not trigger EROFS when the sandbox setup script tries to mkdir those
// targets. Bindings whose target lives outside the worktree (and bindings that
// would escape the worktree via path traversal) are skipped — the sandbox layer
// handles those during normal mount setup.
//
// projectDir is used to expand the ${PROJECT_WORKDIR} token. ${WORKTREE}
// expands to worktreePath. Other tokens are passed through unchanged (matching
// expandWorktreeBindings, which keeps them literal for debuggability).
//
// Idempotent: dirs that already exist are left alone (os.MkdirAll). Safe to
// call after either Create or Recreate.
func (m *WorktreeManager) EnsureBindingTargets(worktreePath string, bindings []orchestrator.BindMount, projectDir string) error {
	if worktreePath == "" || len(bindings) == 0 {
		return nil
	}
	cleanWT := filepath.Clean(worktreePath)
	for _, bm := range expandWorktreeBindings(bindings, worktreePath, projectDir) {
		target := bm.Target
		if target == "" {
			target = bm.Source
		}
		if target == "" {
			continue
		}
		cleanTarget := filepath.Clean(target)
		// Reject bindings that don't actually live under the worktree (including
		// path-traversal escapes after Clean). Skipping is fine — those targets
		// either resolve outside the ro bind or the sandbox layer creates them.
		if !isUnderDir(cleanTarget, cleanWT) {
			continue
		}
		dirToCreate := cleanTarget
		if bm.IsFile {
			dirToCreate = filepath.Dir(cleanTarget)
		}
		// Final guard: after stripping a filename, dirToCreate must still be
		// strictly under the worktree (i.e. not the worktree root itself, which
		// already exists, and not a parent).
		if !isAtOrUnderDir(dirToCreate, cleanWT) {
			continue
		}
		if dirToCreate == cleanWT {
			// Nothing to do — worktree root already exists.
			continue
		}
		if err := os.MkdirAll(dirToCreate, 0o755); err != nil {
			return fmt.Errorf("pre-mkdir additional_bindings target %q: %w", dirToCreate, err)
		}
	}
	return nil
}

// isUnderDir reports whether path is strictly inside parent (parent itself
// returns false). Both inputs are expected to be filepath.Clean'd.
func isUnderDir(path, parent string) bool {
	if path == parent {
		return false
	}
	if parent == "" {
		return false
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}

// isAtOrUnderDir reports whether path is either parent or strictly inside it.
// Used as the final mkdir guard so we never traverse out of the worktree.
func isAtOrUnderDir(path, parent string) bool {
	if path == parent {
		return true
	}
	return isUnderDir(path, parent)
}

func (m *WorktreeManager) CleanOrphaned(resolve func(taskID, projectID string) (string, string, error)) error {
	active, err := ListActiveWorktrees(m.DB)
	if err != nil {
		return err
	}

	for _, w := range active {
		status, projectDir, err := resolve(w.TaskID, w.ProjectID)
		if err != nil {
			slog.Warn("orphan lookup failed", "task_id", w.TaskID, "project_id", w.ProjectID, "error", err)
			continue
		}
		if status != "done" && status != "aborted" {
			continue
		}

		// Same rationale as CleanupForTask: don't delete the boid/<id8> branch
		// here. The parent supervisor's finalizeTerminal (or, eventually, GC)
		// drops the branch once it's no longer needed.
		if err := m.Remove(projectDir, w.TaskID, false); err != nil {
			slog.Warn("orphan cleanup failed", "task_id", w.TaskID, "error", err)
		}
	}
	return nil
}
