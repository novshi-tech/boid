package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// performClone executes the sandbox-internal clone + branch-resolution
// launch sequence declared by cs (docs/plans/git-gateway-cutover.md PR5:
// 「runner の clone 実行機構 + branch 宣言」). It is a no-op when
// cs.Enabled is false — the default for every sandbox.Spec until the PR6
// cutover engages this path.
//
// This is pure exec.Command plumbing (no syscalls), unlike the mount /
// pivot_root machinery in runner_linux.go, so it lives in this
// build-tag-free file and can be unit tested off the syscall path — see
// clone_test.go.
//
// reopen = re-running this exact sequence is intentional and idempotent: an
// existing TargetDir is wiped and re-cloned from scratch rather than
// fetched-in-place, mirroring the plan's "reopen = 同シーケンス再実行" /
// "保証は commit(+push) 済みのみ" decision
// (docs/plans/container-based-boid.md 「決定: clone の置き場所と reopen 意味論」).
//
// Every error this function returns has already been passed through
// redactCloneURLToken: git's own stderr (and this function's own error
// messages) can otherwise echo cs.URL — which embeds a live gateway job
// token — verbatim, and that text is exactly what callers pass to
// State.Fail / print to the runner's stderr on failure
// (docs/plans/git-gateway-cutover.md 「落とし穴・注意」: token redact は
// 診断出力全般が対象). Redacting once here, at the single exit point, means
// every call site (runner-state.json, the runner-inner-child stderr line)
// is covered without having to remember to redact individually.
func performClone(cs sandbox.CloneSpec, st *State) error {
	if err := performCloneSteps(cs, st); err != nil {
		return errors.New(redactCloneURLToken(err.Error()))
	}
	return nil
}

func performCloneSteps(cs sandbox.CloneSpec, st *State) error {
	if !cs.Enabled {
		return nil
	}
	if cs.URL == "" || cs.TargetDir == "" || cs.Branch == "" || cs.BaseBranch == "" {
		return fmt.Errorf("runner clone: spec.Clone is enabled but URL/TargetDir/Branch/BaseBranch must all be set (url=%q target=%q branch=%q base_branch=%q)",
			cs.URL, cs.TargetDir, cs.Branch, cs.BaseBranch)
	}

	git := cs.RealGitBin
	if git == "" {
		git = "git"
	}

	// reopen: idempotent by re-clone. Wipe any leftover TargetDir from a
	// previous attempt (or a prior job invocation reusing the same sandbox
	// root) before cloning fresh.
	if err := os.RemoveAll(cs.TargetDir); err != nil {
		return fmt.Errorf("runner clone: remove existing target dir %s: %w", cs.TargetDir, err)
	}

	args := []string{"clone"}
	referenceUsed := false
	if cs.ReferenceDir != "" {
		if info, err := os.Stat(cs.ReferenceDir); err == nil && info.IsDir() {
			args = append(args, "--reference", cs.ReferenceDir)
			referenceUsed = true
		}
		// Missing reference dir: graceful degradation per the plan — clone
		// proceeds without --reference rather than failing.
	}
	args = append(args, cs.URL, cs.TargetDir)

	if out, err := runGit(git, "", args...); err != nil {
		return fmt.Errorf("runner clone: git clone (reference_used=%v): %w\n%s", referenceUsed, err, strings.TrimSpace(out))
	}
	st.OK("inner-child", "clone-fetch")

	if err := resolveCloneBranch(git, cs, st); err != nil {
		return err
	}
	st.OK("inner-child", "clone-branch-resolve")
	return nil
}

// resolveCloneBranch performs the runner-side counterpart of
// dispatcher.WorktreeManager.Create's branch resolution
// (internal/dispatcher/worktree_manager.go), operating on a fresh clone
// rather than a long-lived host repo. Because the clone is fresh, no local
// branches other than the checked-out default branch exist yet, so — unlike
// the worktree side — there is no "a stale local branch already exists" case
// to reconcile; every candidate is either a remote-tracking ref `git clone`
// already fetched, or a brand-new local branch this function creates.
func resolveCloneBranch(git string, cs sandbox.CloneSpec, st *State) error {
	dir := cs.TargetDir
	baseLocal := strings.TrimPrefix(cs.BaseBranch, "origin/")
	baseRef := "origin/" + baseLocal

	if _, err := runGit(git, dir, "rev-parse", "--verify", "--quiet", baseRef); err != nil {
		// Case 3 equivalent (dispatcher.WorktreeManager.ensureBaseBranchExists):
		// BaseBranch exists on neither origin nor locally yet. Create it
		// from BaseBranchForkPoint or refs/remotes/origin/HEAD.
		start, source, ferr := resolveCloneForkStart(git, dir, cs.BaseBranchForkPoint)
		if ferr != nil {
			return fmt.Errorf("runner clone: resolve base branch %q: %w", cs.BaseBranch, ferr)
		}
		if _, cerr := runGit(git, dir, "branch", baseLocal, start); cerr != nil {
			return fmt.Errorf("runner clone: create base branch %q from %q (%s): %w", baseLocal, start, source, cerr)
		}
		baseRef = baseLocal
		st.OK("inner-child", "clone-base-branch-created")
	}

	if cs.CheckoutOnly {
		// Root task: occupy Branch (== BaseBranch by dispatcher's
		// declaration contract) directly.
		if _, err := runGit(git, dir, "checkout", "-B", cs.Branch, baseRef); err != nil {
			return fmt.Errorf("runner clone: checkout -B %s %s: %w", cs.Branch, baseRef, err)
		}
		return nil
	}

	forkStart := baseRef
	if cs.ForkPoint != "" {
		resolved, err := resolveCloneRef(git, dir, cs.ForkPoint)
		if err != nil {
			return fmt.Errorf("runner clone: resolve fork point %q: %w", cs.ForkPoint, err)
		}
		forkStart = resolved
	}
	if _, err := runGit(git, dir, "checkout", "-B", cs.Branch, forkStart); err != nil {
		return fmt.Errorf("runner clone: checkout -B %s %s: %w", cs.Branch, forkStart, err)
	}
	return nil
}

// resolveCloneForkStart picks the start point for creating BaseBranch
// locally when it does not exist on origin (or locally) yet. Mirrors
// dispatcher.WorktreeManager.resolveCase3ForkStart, minus the pre-fetch: a
// fresh clone already has every remote branch's objects and refs, so there
// is nothing left to fetch.
func resolveCloneForkStart(git, dir, forkPoint string) (start, source string, err error) {
	if forkPoint != "" {
		if _, verr := runGit(git, dir, "rev-parse", "--verify", "--quiet", forkPoint); verr == nil {
			return forkPoint, "project.yaml fork_point", nil
		}
		return "", "", fmt.Errorf("fork_point %q does not resolve in the clone", forkPoint)
	}
	if _, verr := runGit(git, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD"); verr == nil {
		return "refs/remotes/origin/HEAD", "origin/HEAD", nil
	}
	return "", "", fmt.Errorf("no fork point available: fork_point unset and refs/remotes/origin/HEAD is not resolvable (upstream may not advertise a default branch)")
}

// resolveCloneRef resolves an arbitrary ref/branch-name declaration (as used
// for ForkPoint) against the fresh clone at dir. It mirrors
// dispatcher.WorktreeManager.resolveForkPoint's remote-backed case, minus
// the "boid/<id8>" local-only-branch special case: worktree-local boid/<id8>
// branches live only in a host worktree and are never pushed, so a fresh
// clone (which only ever sees origin's pushed refs) genuinely cannot
// resolve them until the referenced branch has been pushed — a documented
// consequence of the clone model's "push only" sharing semantics
// (docs/plans/container-based-boid.md 「意味論の変化」), not a bug here.
func resolveCloneRef(git, dir, ref string) (string, error) {
	if _, err := runGit(git, dir, "rev-parse", "--verify", "--quiet", ref); err == nil {
		return ref, nil
	}
	if !strings.HasPrefix(ref, "origin/") {
		originRef := "origin/" + ref
		if _, err := runGit(git, dir, "rev-parse", "--verify", "--quiet", originRef); err == nil {
			return originRef, nil
		}
	}
	return "", fmt.Errorf("ref %q not found in clone (only origin's pushed refs are visible to a fresh clone)", ref)
}

// runGit runs git with args, in dir (unless dir is empty, in which case the
// current process cwd is used — e.g. `git clone <url> <target>` needs no
// starting directory since the target is an explicit argument). It returns
// combined stdout+stderr for error messages/diagnostics.
func runGit(git, dir string, args ...string) (string, error) {
	cmd := exec.Command(git, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
