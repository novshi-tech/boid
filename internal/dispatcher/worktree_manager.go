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
// If baseBranch is empty, "main" is used as default.
// If origin/<baseBranch> exists, returns ("origin/<baseBranch>", true, nil).
// If only a local branch exists, returns (baseBranch, false, nil).
// If neither origin/<baseBranch> nor the local branch exists, returns an error.
func (m *WorktreeManager) resolveBaseBranch(projectDir, baseBranch string) (string, bool, error) {
	if baseBranch == "" {
		baseBranch = "main"
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

func (m *WorktreeManager) Create(projectDir, projectID, taskID, branchPrefix, baseBranch string) (*Worktree, error) {
	if branchPrefix == "" {
		branchPrefix = "boid/"
	}

	shortID := taskID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	branch := branchPrefix + shortID
	wtPath := filepath.Join(m.RootDir, projectID, shortID)

	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktree parent: %w", err)
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

	cmd := exec.Command(m.gitBin(), "worktree", "add", "--no-track", "-b", branch, wtPath, resolvedBase)
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

	if deleteBranch {
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

	return m.Remove(projectDir, taskID, true)
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

		if err := m.Remove(projectDir, w.TaskID, true); err != nil {
			slog.Warn("orphan cleanup failed", "task_id", w.TaskID, "error", err)
		}
	}
	return nil
}
