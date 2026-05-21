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
	return r.allocateWorktree(spec)
}

// existingWorktreePath returns the worktree path for a task without creating
// or recreating anything. Gate jobs pass this as the starting hint to
// ensureHostGateWorktree; hook jobs pass it to the broker TokenContext so
// broker-side git commands know where to operate.
func (r *Runner) existingWorktreePath(spec *orchestrator.JobSpec) string {
	if spec == nil || r.Worktrees == nil || spec.TaskID == "" {
		return ""
	}
	w, err := r.Worktrees.Get(spec.TaskID)
	if err != nil || w == nil || w.CleanedAt != nil {
		return ""
	}
	return w.Path
}

func (r *Runner) allocateWorktree(spec *orchestrator.JobSpec) (string, error) {
	if spec == nil || r.Worktrees == nil {
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
		// Re-run case: worktree already on disk. Still pre-mkdir binding targets
		// in case the job's additional_bindings have changed (new kit/version).
		if err := r.Worktrees.EnsureBindingTargets(existing.Path, spec.Visibility.AdditionalBindings, spec.Visibility.ProjectDir); err != nil {
			return "", fmt.Errorf("ensure binding targets: %w", err)
		}
		return existing.Path, nil
	}
	if existing != nil && existing.CleanedAt != nil {
		w, err := r.Worktrees.Recreate(spec.Visibility.ProjectDir, spec.TaskID)
		if err != nil {
			return "", fmt.Errorf("recreate worktree: %w", err)
		}
		if err := r.Worktrees.EnsureBindingTargets(w.Path, spec.Visibility.AdditionalBindings, spec.Visibility.ProjectDir); err != nil {
			return "", fmt.Errorf("ensure binding targets: %w", err)
		}
		return w.Path, nil
	}

	// First-time creation: task metadata is needed for base branch.
	// Branch prefix is hardcoded to "boid/" (Phase 3-1: branch_prefix is
	// no longer configurable per behavior).
	task, err := r.TaskLookup.GetTask(spec.TaskID)
	if err != nil {
		return "", fmt.Errorf("lookup task for worktree: %w", err)
	}
	if task == nil {
		return "", fmt.Errorf("task %q not found for worktree creation", spec.TaskID)
	}
	var createOpts CreateOpts
	if task.ParentID == "" {
		// Root task: occupy the base_branch directly rather than creating a
		// new boid/<id8> branch. This is P2 of the dynamic base-branch overhaul.
		createOpts.CheckoutBranch = task.BaseBranch
	}
	w, err := r.Worktrees.Create(
		spec.Visibility.ProjectDir,
		spec.ProjectID,
		task.ID,
		task.BaseBranch,
		createOpts,
	)
	if err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	// Pre-mkdir any additional_bindings target dir that lives under the worktree.
	// The worktree is later bind-mounted readonly for readonly:true tasks; doing
	// the mkdir on the host (still writable) before the sandbox setup script
	// runs sidesteps the EROFS trap.
	if err := r.Worktrees.EnsureBindingTargets(w.Path, spec.Visibility.AdditionalBindings, spec.Visibility.ProjectDir); err != nil {
		return "", fmt.Errorf("ensure binding targets: %w", err)
	}
	return w.Path, nil
}
