package dispatcher

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// If origin/<baseBranch> exists, returns ("origin/<baseBranch>", true).
// Otherwise returns (baseBranch, false).
func (m *WorktreeManager) resolveBaseBranch(projectDir, baseBranch string) (string, bool) {
	if baseBranch == "" {
		baseBranch = "main"
	}
	if strings.HasPrefix(baseBranch, "origin/") {
		return baseBranch, true
	}
	cmd := exec.Command(m.gitBin(), "rev-parse", "--verify", "origin/"+baseBranch)
	cmd.Dir = projectDir
	if err := cmd.Run(); err == nil {
		return "origin/" + baseBranch, true
	}
	return baseBranch, false
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

	resolvedBase, shouldFetch := m.resolveBaseBranch(projectDir, baseBranch)

	if shouldFetch {
		branchToFetch := strings.TrimPrefix(resolvedBase, "origin/")
		fetchCmd := exec.Command(m.gitBin(), "fetch", "origin", branchToFetch)
		fetchCmd.Dir = projectDir
		if out, err := fetchCmd.CombinedOutput(); err != nil {
			slog.Warn("git fetch failed, falling back to local branch",
				"branch", branchToFetch, "error", err, "output", strings.TrimSpace(string(out)))
			resolvedBase = branchToFetch
		}
	}

	cmd := exec.Command(m.gitBin(), "worktree", "add", "-b", branch, wtPath, resolvedBase)
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git worktree add: %w\n%s", err, out)
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
	fetchCmd := exec.Command(m.gitBin(), "-C", projectDir, "fetch", "origin", w.Branch)
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git fetch origin %s: %w\n%s", w.Branch, err, strings.TrimSpace(string(out)))
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
	} else {
		// Local branch was deleted; recreate it from remote.
		wtCmd = exec.Command(m.gitBin(), "worktree", "add", "-B", w.Branch, w.Path, "origin/"+w.Branch)
		wtCmd.Dir = projectDir
	}
	if out, err := wtCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	if err := ClearWorktreeCleaned(m.DB, taskID); err != nil {
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", w.Path).Run()
		return nil, fmt.Errorf("clear worktree cleaned: %w", err)
	}

	w.CleanedAt = nil
	slog.Info("worktree recreated", "task_id", taskID, "path", w.Path, "branch", w.Branch)
	return w, nil
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
