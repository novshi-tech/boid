package orchestrator

// BuildCloneDeclaration derives the sandbox-internal clone branch declaration
// for a task (docs/plans/git-gateway-cutover.md PR6 cutover;
// docs/plans/branch-policy-simplification.md Phase 1). Dispatcher no longer
// runs any git command against a host repo — it just carries this
// declaration through to the sandbox, and the runner resolves it for real
// after cloning (docs/plans/git-gateway-cutover.md 「5. branch 宣言の JobSpec
// 化」).
//
// Every task — root or child, worktree or not — checks out task.BaseBranch
// directly inside its sandbox-internal clone (CheckoutOnly is always true
// now). The per-task "boid/<id8>" branch and its fork-point, worktree-era
// concepts are retired here: they existed only to let a child task continue
// from a parent's uncommitted work on a shared .git. Cutover's
// independent-fresh-clone-per-job model makes that both impossible (a fresh
// clone only ever sees origin's already-pushed refs — an uncommitted parent
// branch is invisible to it) and unnecessary (the clone itself is now the
// isolation unit, so no two tasks ever contend for the same branch name
// inside their own sandbox). See
// docs/plans/branch-policy-simplification.md for the full rationale and the
// readonly-supervisor design mismatch this removal fixes.
//
// baseBranchForkPoint is ProjectMeta.ForkPoint (the ClassifyBaseBranch case-3
// fork point: the start point used to create task.BaseBranch locally when it
// exists on neither origin nor locally yet), passed straight through to
// CloneDeclaration.BaseBranchForkPoint. This is a separate, independent axis
// from the retired per-task fork point described above — see
// CloneDeclaration.BaseBranchForkPoint's doc comment.
func BuildCloneDeclaration(task *Task, baseBranchForkPoint string) *CloneDeclaration {
	if task == nil {
		return nil
	}
	return &CloneDeclaration{
		Branch:              task.BaseBranch,
		BaseBranch:          task.BaseBranch,
		CheckoutOnly:        true,
		BaseBranchForkPoint: baseBranchForkPoint,
	}
}
