package orchestrator

import (
	"fmt"
	"os/exec"
	"strings"
)

// BaseBranchState describes the relationship between the project's working
// directory HEAD and a task's resolved base_branch. The three cases drive
// Phase 2-2 supervisor execution location routing:
//
//   - Case1HeadMatches:        project dir is already on baseBranch
//     → supervisor runs in project dir directly (worktree=false).
//   - Case2ExistsButNotCheckedOut: baseBranch exists locally or on origin but
//     project HEAD is on a different branch
//     → supervisor needs a worktree that checks baseBranch out.
//   - Case3NotFound:           baseBranch is unknown to both local and origin
//     → supervisor needs a worktree whose base branch must be created from
//     the current project HEAD before the worktree can be allocated.
//
// Detached-HEAD projects cannot serve as a sensible default for any of these
// cases (case 3 in particular needs a real branch to derive from), so
// classification reports the dedicated detached-HEAD error rather than
// guessing.
type BaseBranchState int

const (
	// BaseBranchStateUnknown is the zero value. It only appears when an error
	// is returned; callers should never act on it directly.
	BaseBranchStateUnknown BaseBranchState = iota
	// Case1HeadMatches: project HEAD == baseBranch. Worktree is unnecessary;
	// the supervisor can run directly in the project dir.
	Case1HeadMatches
	// Case2ExistsButNotCheckedOut: baseBranch resolves to a ref (local or
	// origin/<baseBranch>) but is not what the project HEAD points at.
	Case2ExistsButNotCheckedOut
	// Case3NotFound: baseBranch does not exist locally and origin/<baseBranch>
	// is unknown to the project repo. Callers that own the project dir create
	// the branch from HEAD before allocating a worktree.
	Case3NotFound
)

// String makes BaseBranchState printable for log lines / test failure
// messages. Not part of the public contract — callers should switch on the
// enum value, not on the rendered string.
func (s BaseBranchState) String() string {
	switch s {
	case Case1HeadMatches:
		return "case1_head_matches"
	case Case2ExistsButNotCheckedOut:
		return "case2_exists_but_not_checked_out"
	case Case3NotFound:
		return "case3_not_found"
	default:
		return "unknown"
	}
}

// ErrDetachedHead is returned by ClassifyBaseBranch when the project working
// directory is in detached-HEAD state. Wrapped so callers can use errors.Is.
var ErrDetachedHead = fmt.Errorf("project is in detached HEAD state")

// ClassifyBaseBranch inspects the git repository at projectDir to decide how
// a task whose resolved base_branch is baseBranch should be scheduled. See
// BaseBranchState for the three case definitions.
//
// baseBranch must already be expanded (no ${...} templates). An empty
// baseBranch is treated as "main" to match the existing worktree manager
// default — the alternative would be to refuse, but that makes the call site
// in CreateTask more painful for no benefit.
//
// The classify call is read-only: no branches are created, no fetches are
// issued, and the project HEAD is not mutated. Case 3 mitigation (creating
// the branch) is the caller's responsibility; ClassifyBaseBranch only
// reports the state.
func ClassifyBaseBranch(projectDir, baseBranch string) (BaseBranchState, error) {
	if projectDir == "" {
		return BaseBranchStateUnknown, fmt.Errorf("ClassifyBaseBranch: projectDir is required")
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	// origin/<branch> is treated as a remote-tracking reference: there is no
	// way for HEAD to equal it (HEAD is always a local branch or detached),
	// so the only relevant question is whether the ref resolves.
	stripped := strings.TrimPrefix(baseBranch, "origin/")

	headBranch, detached, err := currentHeadBranch(projectDir)
	if err != nil {
		return BaseBranchStateUnknown, err
	}
	if detached {
		return BaseBranchStateUnknown, fmt.Errorf("ClassifyBaseBranch: %w", ErrDetachedHead)
	}

	// Case 1: HEAD is on the requested branch. We compare against the stripped
	// form so "origin/main" and "main" both match a project on local "main".
	if headBranch == stripped {
		return Case1HeadMatches, nil
	}

	// Case 2: ref exists either locally or on origin (do not fetch — the
	// repo's existing remote view is what matters).
	if refExists(projectDir, stripped) || refExists(projectDir, "origin/"+stripped) {
		return Case2ExistsButNotCheckedOut, nil
	}

	return Case3NotFound, nil
}

// currentHeadBranch returns the local branch name HEAD points at. When HEAD
// is detached the second return value is true and the branch string is
// empty.
func currentHeadBranch(projectDir string) (string, bool, error) {
	// symbolic-ref errors out on detached HEAD, which is exactly what we want
	// to distinguish from "branch named HEAD" (impossible) or transient git
	// failures.
	cmd := exec.Command("git", "-C", projectDir, "symbolic-ref", "--quiet", "HEAD")
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		return strings.TrimPrefix(ref, "refs/heads/"), false, nil
	}
	// symbolic-ref --quiet returns exit code 1 with empty stderr for detached
	// HEAD. Anything else (e.g. not a git repo) is an actual error.
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return "", true, nil
	}
	return "", false, fmt.Errorf("git -C %s symbolic-ref HEAD: %w", projectDir, err)
}

// refExists reports whether the given ref resolves in projectDir. Errors are
// folded into a "no" so the classifier degrades gracefully when a ref simply
// does not exist (which is the most common case and not exceptional).
func refExists(projectDir, ref string) bool {
	if ref == "" {
		return false
	}
	cmd := exec.Command("git", "-C", projectDir, "rev-parse", "--verify", "--quiet", ref)
	return cmd.Run() == nil
}
