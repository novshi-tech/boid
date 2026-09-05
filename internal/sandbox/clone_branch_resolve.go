package sandbox

import (
	"fmt"
	"strings"
)

// GitRunFunc runs a single git subcommand (args, without the leading "git")
// against a directory a closure captures, returning any error from the
// underlying process. Both internal/dispatcher's and internal/sandbox/
// runner's own git-exec primitives can be adapted to this shape with a
// one-line closure — see their respective call sites of
// ResolveCloneBranchRef below.
type GitRunFunc func(args ...string) error

// ResolveCloneBranchRef resolves and checks out a task's working branch in a
// freshly cloned repository, sharing the exact algorithm between the
// sandbox-internal runner path (internal/sandbox/runner/clone.go's
// resolveCloneBranch) and the daemon-side per-job clone path
// (internal/dispatcher/checkout.go's PrepareJobCheckout).
//
// branch is the ref ultimately checked out; baseBranch is the ref whose tip
// provides the starting point — identical to branch for every current
// production caller, but kept as a separate parameter rather than collapsed
// away, so the declaration shape stays self-describing. Both may
// legitimately carry an "origin/" prefix (a project.yaml `base_branch:
// origin/main`) — this function strips it consistently from both before
// creating/checking out the local branch, so the resulting local branch is
// always named "main", never "origin/main".
//
// baseBranchForkPoint, when set, is used to create baseBranch locally if it
// resolves on neither origin nor locally yet (ClassifyBaseBranch case 3,
// mirrored here since a fresh clone has no local branches yet beyond the
// default one `git clone` itself checked out); empty falls back to
// refs/remotes/origin/HEAD.
//
// run executes a git subcommand against the target working directory (the
// caller's closure captures both the directory and which git binary to
// invoke); this function performs no I/O itself beyond calling run, so it
// stays unit-testable with a fake run that never shells out to a real git
// binary.
//
// Returns whether a new local base branch was created from a fork point
// (created == true) — callers that emit their own diagnostics on that event
// (runner-state.json's "clone-base-branch-created" marker) use this instead
// of re-deriving it.
func ResolveCloneBranchRef(run GitRunFunc, branch, baseBranch, baseBranchForkPoint string) (created bool, err error) {
	if branch == "" || baseBranch == "" {
		return false, fmt.Errorf("resolve clone branch: branch and base_branch must both be set (branch=%q base_branch=%q)", branch, baseBranch)
	}
	branchLocal := strings.TrimPrefix(branch, "origin/")
	baseLocal := strings.TrimPrefix(baseBranch, "origin/")
	baseRef := "origin/" + baseLocal

	if verifyErr := run("rev-parse", "--verify", "--quiet", baseRef); verifyErr != nil {
		// ClassifyBaseBranch case 3: baseBranch exists on neither origin nor
		// locally yet. Create it from baseBranchForkPoint or
		// refs/remotes/origin/HEAD.
		start, source, ferr := resolveCloneForkStart(run, baseBranchForkPoint)
		if ferr != nil {
			return false, fmt.Errorf("resolve base branch %q: %w", baseBranch, ferr)
		}
		if cerr := run("branch", baseLocal, start); cerr != nil {
			return false, fmt.Errorf("create base branch %q from %q (%s): %w", baseLocal, start, source, cerr)
		}
		baseRef = baseLocal
		created = true
	}

	if err := run("checkout", "-B", branchLocal, baseRef); err != nil {
		return created, fmt.Errorf("checkout -B %s %s: %w", branchLocal, baseRef, err)
	}
	return created, nil
}

// resolveCloneForkStart picks the start point for creating baseBranch
// locally when it does not exist on origin (or locally) yet. No pre-fetch is
// needed: a fresh clone already has every remote branch's objects and refs.
func resolveCloneForkStart(run GitRunFunc, forkPoint string) (start, source string, err error) {
	if forkPoint != "" {
		if verr := run("rev-parse", "--verify", "--quiet", forkPoint); verr == nil {
			return forkPoint, "project.yaml fork_point", nil
		}
		return "", "", fmt.Errorf("fork_point %q does not resolve in the clone", forkPoint)
	}
	if verr := run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD"); verr == nil {
		return "refs/remotes/origin/HEAD", "origin/HEAD", nil
	}
	return "", "", fmt.Errorf("no fork point available: fork_point unset and refs/remotes/origin/HEAD is not resolvable (upstream may not advertise a default branch)")
}
