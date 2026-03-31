package worktree

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Manager handles git worktree lifecycle for task isolation.
type Manager struct {
	RootDir string // e.g. ~/.local/share/boid/worktrees
	DB      *db.DB
	GitBin  string // path to git binary; defaults to "git"
}

func (m *Manager) gitBin() string {
	if m.GitBin != "" {
		return m.GitBin
	}
	return "git"
}

// Create creates a git worktree for the given task.
// projectDir is the host-side project directory (the main git repo).
// Returns the created worktree record.
func (m *Manager) Create(projectDir, projectID, taskID, branchPrefix, baseBranch string) (*Worktree, error) {
	if branchPrefix == "" {
		branchPrefix = "boid/"
	}
	if baseBranch == "" {
		baseBranch = "HEAD"
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

	cmd := exec.Command(m.gitBin(), "worktree", "add", "-b", branch, wtPath, baseBranch)
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
		BaseBranch: baseBranch,
	}
	if err := CreateWorktree(m.DB.Conn, w); err != nil {
		// Best-effort cleanup on DB failure
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", wtPath).Run()
		return nil, fmt.Errorf("record worktree: %w", err)
	}

	slog.Info("worktree created", "task_id", taskID, "path", wtPath, "branch", branch)
	return w, nil
}

// Remove removes the git worktree and optionally deletes the local branch.
// deleteBranch should be true for aborted tasks, false for done tasks
// (where the branch lives on as a PR).
func (m *Manager) Remove(projectDir, taskID string, deleteBranch bool) error {
	w, err := GetWorktreeByTask(m.DB.Conn, taskID)
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}
	if w == nil || w.CleanedAt != nil {
		return nil // already cleaned or never existed
	}

	// git worktree remove
	cmd := exec.Command(m.gitBin(), "-C", projectDir, "worktree", "remove", "--force", w.Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("git worktree remove failed, attempting manual cleanup",
			"error", err, "output", strings.TrimSpace(string(out)))
		os.RemoveAll(w.Path)
		// Also prune stale worktree entries
		exec.Command(m.gitBin(), "-C", projectDir, "worktree", "prune").Run()
	}

	if deleteBranch {
		cmd := exec.Command(m.gitBin(), "-C", projectDir, "branch", "-D", w.Branch)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("git branch -D failed", "branch", w.Branch,
				"error", err, "output", strings.TrimSpace(string(out)))
		}
	}

	if err := MarkWorktreeCleaned(m.DB.Conn, taskID); err != nil {
		return fmt.Errorf("mark cleaned: %w", err)
	}

	slog.Info("worktree removed", "task_id", taskID, "path", w.Path, "branch_deleted", deleteBranch)
	return nil
}

// Get returns the worktree for the given task, or nil if none exists.
func (m *Manager) Get(taskID string) (*Worktree, error) {
	return GetWorktreeByTask(m.DB.Conn, taskID)
}

// CleanupForTask removes the worktree for a task that reached a terminal state.
// For "done" tasks, the branch is kept (it lives on as a PR).
// For "aborted" tasks, the branch is deleted.
func (m *Manager) CleanupForTask(taskID string, newStatus orchestrator.TaskStatus) error {
	if newStatus != orchestrator.TaskStatusDone && newStatus != orchestrator.TaskStatusAborted {
		return nil
	}

	w, err := m.Get(taskID)
	if err != nil {
		return err
	}
	if w == nil || w.CleanedAt != nil {
		return nil
	}

	proj, err := orchestrator.GetProject(m.DB.Conn, w.ProjectID)
	if err != nil {
		return fmt.Errorf("get project for worktree cleanup: %w", err)
	}

	deleteBranch := newStatus == orchestrator.TaskStatusAborted
	return m.Remove(proj.WorkDir, taskID, deleteBranch)
}

// CleanOrphaned removes worktrees whose tasks have reached terminal states
// but were not cleaned up (e.g., due to a crash).
func (m *Manager) CleanOrphaned() error {
	active, err := ListActiveWorktrees(m.DB.Conn)
	if err != nil {
		return err
	}

	for _, w := range active {
		task, err := orchestrator.GetTask(m.DB.Conn, w.TaskID)
		if err != nil {
			slog.Warn("orphan check: task lookup failed", "task_id", w.TaskID, "error", err)
			continue
		}
		if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
			continue
		}

		proj, err := orchestrator.GetProject(m.DB.Conn, w.ProjectID)
		if err != nil {
			slog.Warn("orphan check: project lookup failed", "project_id", w.ProjectID, "error", err)
			continue
		}

		deleteBranch := task.Status == orchestrator.TaskStatusAborted
		if err := m.Remove(proj.WorkDir, w.TaskID, deleteBranch); err != nil {
			slog.Warn("orphan cleanup failed", "task_id", w.TaskID, "error", err)
		}
	}
	return nil
}
