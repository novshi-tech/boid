package orchestrator

// BuildCloneDeclaration derives the sandbox-internal clone branch
// declaration for a task. Dispatcher no longer runs any git command against
// a host repo — it just carries this declaration through to the sandbox,
// and the runner resolves it for real after cloning.
//
// Every task — root or child, worktree or not — checks out task.BaseBranch
// directly inside its sandbox-internal clone (CheckoutOnly is always true
// now). There is no per-task branch or fork-point: the clone itself is the
// isolation unit, so no two tasks ever contend for the same branch name
// inside their own sandbox.
//
// baseBranchForkPoint is ProjectMeta.ForkPoint (the ClassifyBaseBranch case-3
// fork point: the start point used to create task.BaseBranch locally when it
// exists on neither origin nor locally yet), passed straight through to
// CloneDeclaration.BaseBranchForkPoint.
func BuildCloneDeclaration(task *Task, baseBranchForkPoint string) *CloneDeclaration {
	if task == nil || task.Exec == nil {
		return nil
	}
	return &CloneDeclaration{
		Branch:              task.Exec.BaseBranch,
		BaseBranch:          task.Exec.BaseBranch,
		CheckoutOnly:        true,
		BaseBranchForkPoint: baseBranchForkPoint,
	}
}
