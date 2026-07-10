package orchestrator

// ComputeHeadBranch returns the HEAD branch that the given task will occupy
// when its worktree is created.
//
// Root tasks (ParentID == "") occupy the project's base_branch directly.
// Child tasks (ParentID != "") get an isolated "boid/<id8>" branch derived
// from the first 8 characters of their task ID.
//
// Used by BranchLockManager to derive the compound lock key so that tasks on
// the same HEAD branch are serialized while tasks on different branches run
// in parallel. For the worktree resolver's fork-point computation (which has
// different semantics for worktree-less parents), use ComputeForkPoint.
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

// ComputeForkPoint returns the branch a child task should fork its worktree
// from, given the parent task. It differs from ComputeHeadBranch in that
// worktree-less parents (Worktree == false) return their BaseBranch rather
// than the synthetic "boid/<id8>" branch — because a worktree=false parent
// runs in the host project dir on its base_branch and never creates a
// boid/<id8> branch. Falling back to base_branch is required for the Phase
// 2-2 supervisor case 1 path (host HEAD already on base_branch); otherwise
// child worktree creation fails with "fork point boid/<id8> not found
// locally (parent task worktree missing?)".
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
// for a task (docs/plans/git-gateway-cutover.md PR6 cutover). It replaces
// dispatcher's host-repo worktree resolver (internal/dispatcher/worktree_resolver.go
// allocateWorktree) for real dispatch: the same root-vs-child resolution
// rules apply, but only as a *declaration* — dispatcher no longer runs any
// git command against the host repo to build it, and the runner resolves it
// for real after cloning (docs/plans/git-gateway-cutover.md 「5. branch 宣言の
// JobSpec 化」).
//
// CheckoutOnly (occupy BaseBranch directly, no new branch) applies both to
// root tasks (ParentID == "") and to any task whose behavior declared
// worktree: false. Pre-cutover, worktree=false meant "run directly in the
// project dir on its base_branch, create no boid/<id8> branch" — which
// relied on the host HEAD already sitting on BaseBranch (enforced by the now-
// retired Phase 2-2 case 1 HEAD guard, dispatcher.EnforceHeadOnBaseBranch).
// Under the clone model there is no host HEAD to rely on: CheckoutOnly simply
// resolves BaseBranch fresh in the clone every time, which is what the old
// invariant was standing in for in the first place.
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
	// Cross-project guard mirrors allocateWorktree's: ComputeForkPoint(parent)
	// names a branch that only resolves in the parent's own repository. When
	// the child lives in a different project, leave ForkPoint empty so the
	// runner falls back to resolving BaseBranch itself.
	if parent != nil && parent.ProjectID == task.ProjectID {
		decl.ForkPoint = ComputeForkPoint(parent)
	}
	return decl
}
