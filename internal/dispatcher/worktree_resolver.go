package dispatcher

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// resolveWorktree returns the host path of the worktree to bind into the
// sandbox, creating or re-creating it via WorktreeManager as needed. If the
// task is not worktree-enabled or the manager is missing, it returns "".
//
// This lives in dispatcher (not orchestrator) because it is an execution
// concern: orchestrator only expresses "this job wants a worktree"; the
// machinery that allocates one belongs next to the sandbox launch.
func (r *Runner) resolveWorktree(spec *orchestrator.JobSpec) (string, error) {
	if spec == nil || !spec.Visibility.UseWorktree || r.Worktrees == nil {
		return "", nil
	}
	if spec.TaskID == "" || spec.ProjectID == "" {
		return "", nil
	}

	existing, err := r.Worktrees.Get(spec.TaskID)
	if err != nil {
		return "", fmt.Errorf("lookup worktree: %w", err)
	}
	if existing != nil && existing.CleanedAt == nil {
		return existing.Path, nil
	}
	if existing != nil && existing.CleanedAt != nil {
		w, err := r.Worktrees.Recreate(spec.Visibility.ProjectDir, spec.TaskID)
		if err != nil {
			return "", fmt.Errorf("recreate worktree: %w", err)
		}
		return w.Path, nil
	}

	// First-time creation: task metadata is needed for branch name / base.
	task, err := r.TaskLookup.GetTask(spec.TaskID)
	if err != nil {
		return "", fmt.Errorf("lookup task for worktree: %w", err)
	}
	if task == nil {
		return "", fmt.Errorf("task %q not found for worktree creation", spec.TaskID)
	}
	w, err := r.Worktrees.Create(
		spec.Visibility.ProjectDir,
		spec.ProjectID,
		task.ID,
		task.BranchPrefix,
		task.BaseBranch,
	)
	if err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	return w.Path, nil
}
