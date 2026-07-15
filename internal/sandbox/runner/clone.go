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

	// reopen: idempotent by re-clone. Wipe any leftover TargetDir contents
	// from a previous attempt (or a prior job invocation reusing the same
	// sandbox root) before cloning fresh.
	//
	// This clears the *contents* of TargetDir rather than removing TargetDir
	// itself (os.RemoveAll(cs.TargetDir) would attempt that as its final
	// step): TargetDir is the sandbox-internal clone mount point — a
	// name-scoped subdirectory of sandboxCloneTargetDir ("/workspace/<name>",
	// workspace 親化リファクタリング, nose 2026-07-13 decision) — which dispatcher's
	// cloneMounts bind-mounts from a host-backed per-job runtime directory
	// (dispatcher.buildSandboxSpec's RuntimesDir-backed clone workspace —
	// docs/plans/git-gateway-cutover.md PR6 cutover, container-based-boid.md
	// 2026-07-08 decision: "clone lands on a runtime-dir bind mount by
	// default, not tmpfs"). Removing an active mount point's own directory
	// entry is refused by the kernel with EBUSY ("device or resource busy")
	// — this fired on *every* clone-enabled dispatch, reopen or not, once
	// RuntimesDir was configured (the production/e2e default), and was
	// masked by an unrelated e2e/run.sh bug that hid failing scenarios
	// behind a false-positive "pass" (see docs/plans/git-gateway-cutover.md
	// and PR #736). clone_test.go's
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

// resolveCloneBranch resolves and checks out the task's working branch inside
// a fresh sandbox-internal clone (docs/plans/git-gateway-cutover.md PR5/PR6:
// dispatcher declares the branch, the runner resolves it after cloning).
// Because the clone is fresh, no local branches other than the checked-out
// default branch exist yet, so there is no "a stale local branch already
// exists" case to reconcile; every candidate is either a remote-tracking ref
// `git clone` already fetched, or a brand-new local branch this function
// creates.
func resolveCloneBranch(git string, cs sandbox.CloneSpec, st *State) error {
	dir := cs.TargetDir
	baseLocal := strings.TrimPrefix(cs.BaseBranch, "origin/")
	baseRef := "origin/" + baseLocal

	if _, err := runGit(git, dir, "rev-parse", "--verify", "--quiet", baseRef); err != nil {
		// ClassifyBaseBranch case 3: BaseBranch exists on neither origin nor
		// locally yet. Create it from BaseBranchForkPoint or
		// refs/remotes/origin/HEAD.
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
		// Every task occupies Branch (== BaseBranch by dispatcher's
		// declaration contract) directly — the per-task "boid/<id8>" branch
		// and its fork-point are retired
		// (docs/plans/branch-policy-simplification.md Phase 1).
		if _, err := runGit(git, dir, "checkout", "-B", cs.Branch, baseRef); err != nil {
			return fmt.Errorf("runner clone: checkout -B %s %s: %w", cs.Branch, baseRef, err)
		}
		return nil
	}

	// orchestrator.BuildCloneDeclaration always sets CheckoutOnly=true now,
	// so this is unreachable in production dispatch. Kept as an explicit,
	// loud failure (rather than silently falling through to a checkout)
	// in case a future or test-only CloneSpec ever sets CheckoutOnly=false
	// again without restoring a real resolution path.
	return fmt.Errorf("runner clone: CloneSpec.CheckoutOnly is false; per-task fork branches were retired in docs/plans/branch-policy-simplification.md Phase 1")
}

// resolveCloneForkStart picks the start point for creating BaseBranch
// locally when it does not exist on origin (or locally) yet. No pre-fetch is
// needed: a fresh clone already has every remote branch's objects and refs.
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
