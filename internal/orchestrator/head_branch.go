package orchestrator

// ComputeHeadBranch returns the HEAD branch the given task occupies inside
// its sandbox-internal clone.
//
// Root tasks (ParentID == "") occupy the project's base_branch directly.
// Child tasks (ParentID != "") get an isolated "boid/<id8>" branch derived
// from the first 8 characters of their task ID.
//
// Used by BuildCloneDeclaration (below) to fill CloneDeclaration.Branch, and
// by planner.taskBusinessEnv to compute BOID_PARENT_BRANCH for a child's
// executor. For the fork-point computation (which has different semantics
// for worktree-less parents), use ComputeForkPoint.
func ComputeHeadBranch(task *Task) string {
	if task.ParentID == "" {
		return task.BaseBranch
	}
	short := task.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return "boid/" + short
}

// ComputeForkPoint returns the branch a child task should fork from — the
// start point the runner passes to `checkout -B` inside the child's sandbox
// clone. It differs from ComputeHeadBranch in that worktree-less parents
// (Worktree == false) return their BaseBranch rather than the synthetic
// "boid/<id8>" branch, because a worktree=false parent runs directly on its
// base_branch and never creates a boid/<id8> branch — so there is no such
// ref for the child's clone to check out.
func ComputeForkPoint(parent *Task) string {
	if parent == nil {
		return ""
	}
	if !parent.Worktree {
		return parent.BaseBranch
	}
	return ComputeHeadBranch(parent)
}

// BuildCloneDeclaration derives the sandbox-internal clone branch declaration
// for a task (docs/plans/git-gateway-cutover.md PR6 cutover). Dispatcher no
// longer runs any git command against a host repo — it just carries this
// declaration through to the sandbox, and the runner resolves it for real
// after cloning (docs/plans/git-gateway-cutover.md 「5. branch 宣言の JobSpec
// 化」).
//
// CheckoutOnly (occupy BaseBranch directly, no new branch) applies both to
// root tasks (ParentID == "") and to any task whose behavior declared
// worktree: false. Pre-cutover, worktree=false meant "run directly on
// base_branch, create no boid/<id8> branch" — which relied on the project's
// HEAD already sitting on BaseBranch (enforced by the now-retired Phase 2-2
// case 1 HEAD guard). Under the clone model there is no such HEAD to rely
// on: CheckoutOnly simply resolves BaseBranch fresh in the clone every
// time, which is what the old invariant was standing in for in the first
// place.
//
// baseBranchForkPoint is ProjectMeta.ForkPoint / Visibility.ForkPoint (the
// ClassifyBaseBranch case-3 fork point), passed straight through to
// CloneDeclaration.BaseBranchForkPoint.
func BuildCloneDeclaration(task *Task, parent *Task, baseBranchForkPoint string) *CloneDeclaration {
	if task == nil {
		return nil
	}
	decl := &CloneDeclaration{
		BaseBranch:          task.BaseBranch,
		BaseBranchForkPoint: baseBranchForkPoint,
	}
	if task.ParentID == "" || !task.Worktree {
		decl.CheckoutOnly = true
		decl.Branch = task.BaseBranch
		return decl
	}
	decl.Branch = ComputeHeadBranch(task) // "boid/<id8>"
	// Cross-project guard: ComputeForkPoint(parent) names a branch that only
	// resolves in the parent's own repository. When the child lives in a
	// different project, leave ForkPoint empty so the runner falls back to
	// resolving BaseBranch itself.
	if parent != nil && parent.ProjectID == task.ProjectID {
		decl.ForkPoint = ComputeForkPoint(parent)
	}
	return decl
}
