package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// clearDirContents removes every entry inside dir but leaves dir's own
// directory entry in place — unlike os.RemoveAll(dir), which also removes
// dir itself as its final step. Returns nil (nothing to clear) if dir does
// not exist yet (the very first dispatch against a fresh runtime directory).
//
// This matters when dir is itself a mount point: see performCloneSteps's
// call site for why /workspace (the sandbox-internal clone target) always
// is one in production/e2e.
func clearDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// performClone executes the sandbox-internal clone + branch-resolution
// launch sequence declared by cs. It is a no-op when cs.Enabled is false.
//
// This is pure exec.Command plumbing (no syscalls), unlike the mount /
// pivot_root machinery in runner_linux.go, so it lives in this
// build-tag-free file and can be unit tested off the syscall path — see
// clone_test.go.
//
// reopen = re-running this exact sequence is intentional and idempotent: an
// existing TargetDir is wiped and re-cloned from scratch rather than
// fetched-in-place.
//
// Every error this function returns has already been passed through
// redactCloneURLToken: git's own stderr (and this function's own error
// messages) can otherwise echo cs.URL — which embeds a live gateway job
// token — verbatim, and that text is exactly what callers pass to
// State.Fail / print to the runner's stderr on failure. Redacting once
// here, at the single exit point, means every call site is covered without
// having to remember to redact individually.
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

	// reopen: idempotent by re-clone. Wipe any leftover TargetDir contents
	// from a previous attempt (or a prior job invocation reusing the same
	// sandbox root) before cloning fresh.
	//
	// This clears the *contents* of TargetDir rather than removing TargetDir
	// itself (os.RemoveAll(cs.TargetDir) would attempt that as its final
	// step): TargetDir is a bind-mounted clone mount point, and removing an
	// active mount point's own directory entry is refused by the kernel with
	// EBUSY. clone_test.go's
	// TestClearDirContentsPreservesDirEntryButRemovesChildren pins that
	// TargetDir's own directory entry survives.
	if err := clearDirContents(cs.TargetDir); err != nil {
		return fmt.Errorf("runner clone: clear existing target dir %s: %w", cs.TargetDir, err)
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

// resolveCloneBranch resolves and checks out the task's working branch
// inside a fresh sandbox-internal clone: dispatcher declares the branch,
// the runner resolves it after cloning. Because the clone is fresh, no
// local branches other than the checked-out default branch exist yet, so
// there is no "a stale local branch already exists" case to reconcile;
// every candidate is either a remote-tracking ref `git clone` already
// fetched, or a brand-new local branch this function creates.
//
// The actual git-command sequence lives in sandbox.ResolveCloneBranchRef —
// internal/dispatcher/checkout.go's PrepareJobCheckout shares this exact
// algorithm rather than reimplementing a divergent copy of it.
func resolveCloneBranch(git string, cs sandbox.CloneSpec, st *State) error {
	if !cs.CheckoutOnly {
		// orchestrator.BuildCloneDeclaration always sets CheckoutOnly=true now,
		// so this is unreachable in production dispatch. Kept as an explicit,
		// loud failure (rather than silently falling through to a checkout)
		// in case a future or test-only CloneSpec ever sets CheckoutOnly=false
		// again without restoring a real resolution path.
		return fmt.Errorf("runner clone: CloneSpec.CheckoutOnly is false; per-task fork branches were retired in docs/plans/branch-policy-simplification.md Phase 1")
	}

	dir := cs.TargetDir
	run := func(args ...string) error {
		_, err := runGit(git, dir, args...)
		return err
	}
	created, err := sandbox.ResolveCloneBranchRef(run, cs.Branch, cs.BaseBranch, cs.BaseBranchForkPoint)
	if err != nil {
		return fmt.Errorf("runner clone: %w", err)
	}
	if created {
		st.OK("inner-child", "clone-base-branch-created")
	}
	return nil
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
