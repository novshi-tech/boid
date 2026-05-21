package dispatcher

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// hostHEADBranch returns the symbolic HEAD branch of the git repo at projectDir,
// or "" if HEAD is detached or the command fails (detached HEAD = case-1 not applicable).
func hostHEADBranch(gitBin, projectDir string) string {
	out, err := exec.Command(gitBin, "-C", projectDir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

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

	// Case-1 (dynamic-base-branch-overhaul §case-1): when a root executor's
	// base_branch equals the currently checked-out host HEAD branch, reuse the
	// project root directly. git worktree add would fail with "already used by
	// worktree" because the host tree already holds that branch. No DB row is
	// created; cleanup is therefore a noop (Remove returns early when w == nil).
	if task.ParentID == "" {
		if head := hostHEADBranch(r.Worktrees.gitBin(), spec.Visibility.ProjectDir); head != "" && head == task.BaseBranch {
			slog.Info("root executor reusing project root (base==host HEAD)",
				"task_id", task.ID, "branch", head, "project_dir", spec.Visibility.ProjectDir)
			if err := r.Worktrees.EnsureBindingTargets(spec.Visibility.ProjectDir, spec.Visibility.AdditionalBindings, spec.Visibility.ProjectDir); err != nil {
				return "", fmt.Errorf("ensure binding targets (project root reuse): %w", err)
			}
			return spec.Visibility.ProjectDir, nil
		}
	}

	var createOpts CreateOpts
	if task.ParentID == "" {
		// Root task: occupy the base_branch directly rather than creating a
		// new boid/<id8> branch. This is P2 of the dynamic base-branch overhaul.
		createOpts.CheckoutBranch = task.BaseBranch
	} else {
		// Child task: fork from the parent's HEAD branch rather than the shared
		// baseBranch. ComputeHeadBranch(parent) is "boid/<parent_id8>" for
		// child parents, or parent.BaseBranch for root parents (P3).
		parent, parentErr := r.TaskLookup.GetTask(task.ParentID)
		if parentErr != nil {
			return "", fmt.Errorf("lookup parent task %q for fork point: %w", task.ParentID, parentErr)
		}
		if parent == nil {
			return "", fmt.Errorf("parent task %q not found for fork point", task.ParentID)
		}
		createOpts.ForkPoint = orchestrator.ComputeHeadBranch(parent)
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
