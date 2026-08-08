package dispatcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// This file implements the daemon-side per-job clone (docs/plans/
// volume-only-daemon.md §論点b's "採用経路 (per-job clone、 clone 時代整合)",
// PR-2b): given a project's daemon-managed bare repository (internal/
// orchestrator's BareRepoPath, populated/kept fresh by CloneBareRepo/
// FetchBareRepo in bare_repo.go — PR-2a), materialize a fresh, per-job
// working checkout ("staging area") the daemon can hand a job container as
// a real host-path bind mount.
//
// This deliberately does NOT use `git worktree` — the plan doc's §論点b
// explicitly retracts that approach (a worktree's .git file records an
// absolute path back to the bare repo, which only resolves inside the SAME
// container/mount-namespace the bare repo lives in; a DooD sibling job
// container has no such path). A plain clone has no such cross-container
// path dependency: TargetDir's own .git is fully self-contained — see
// PrepareJobCheckout's own doc comment for why this means a `file://`
// clone, not `git clone --reference`.
//
// Unlike bare_repo.go's CloneBareRepo/FetchBareRepo, this package-internal
// clone never talks to a remote forge and therefore needs no
// gitgateway.CredentialProvider at all: bareRepoPath is a local filesystem
// path (the daemon's own already-fetched cache), not a credentialed URL.
// Callers are responsible for calling FetchBareRepo (bare_repo.go) FIRST if
// they want the bare repo's own content refreshed against its remote before
// staging a job off of it (docs/plans/volume-only-daemon.md §論点b step 1)
// — that is a separate, credentialed, best-effort concern this file stays
// out of.

// PrepareJobCheckout materializes a fresh, per-job working checkout of
// branch at stagingDir, cloned from the daemon-managed bare repository at
// bareRepoPath (docs/plans/volume-only-daemon.md §論点b steps 2-3: "daemon
// が job 用 staging area を用意... bare repo から per-job clone... を staging
// area に配置").
//
// Cloning is a plain `git clone file://<bareRepoPath>` — deliberately NOT
// `git clone --reference bareRepoPath` (codex round-1, PR834 Blocker 1):
// `--reference` records bareRepoPath itself in stagingDir's own
// `.git/objects/info/alternates`, which only ever resolves from a process
// that can see bareRepoPath on ITS OWN filesystem — true of the daemon
// process that runs this clone, but NOT true of the job container the
// staging area gets bind-mounted into (the job container mounts only
// stagingDir's own subtree, e.g. `checkouts/<jobID>/<project>/`, never the
// daemon's own data/runtime volume bareRepoPath lives under). A job
// container running `git status`/`git commit`/anything that touches HEAD's
// object graph would fail resolving objects through an alternates path it
// cannot see. The explicit `file://` scheme (rather than a bare local path,
// which git would otherwise still auto-hardlink against the source without
// an alternates entry) forces git's own transport-based clone machinery —
// a real, standalone copy of every object stagingDir needs, no alternates
// file, no shared inodes with bareRepoPath — so stagingDir is fully
// self-contained the moment this function returns and never depends on
// bareRepoPath's own continued existence or mount visibility (see also
// TestPrepareJobCheckout_NoAlternatesDependency).
//
// Branch resolution (which ref to check stagingDir out to) is NOT a plain
// `git checkout -B <branch> origin/<branch>` — it shares
// sandbox.ResolveCloneBranchRef with internal/sandbox/runner/clone.go's
// resolveCloneBranch (PR834 PR-2b round-2 codex review Blocker: the two
// paths had drifted — this function's own pre-fix naive `origin/<branch>`
// construction neither stripped an "origin/" prefix baseBranch/branch might
// carry, e.g. a project.yaml `base_branch: origin/main`, which produced an
// unresolvable "origin/origin/main", nor created a missing base branch from
// baseBranchForkPoint the ClassifyBaseBranch case-3 way resolveCloneBranch
// already did). See ResolveCloneBranchRef's own doc comment for the exact
// algorithm; the net effect is the same idempotent-by-reclone contract the
// in-sandbox clone sequence already established (docs/plans/
// branch-policy-simplification.md: reopen = re-running this exact
// sequence), forcing the local branch to exactly match the resolved ref,
// discarding any local drift a same-named branch might otherwise carry from
// `git clone`'s own default-branch checkout.
//
// A prior stagingDir (a leftover from an earlier attempt, or a reopen of
// the same job) is wiped before cloning fresh — the daemon-side analogue of
// clearDirContents/performCloneSteps' own reopen contract. Unlike
// performCloneSteps (which must preserve TargetDir's own directory ENTRY
// because it is an active sandbox mount point the kernel refuses to
// rmdir), stagingDir here is plain daemon-local filesystem the daemon
// itself owns outright before any container ever mounts it, so a full
// os.RemoveAll (removing stagingDir itself, not just its contents) is safe
// and simpler.
//
// remoteURL, when non-empty, is set as stagingDir's own `origin` remote
// after cloning (docs/plans/volume-only-daemon.md §論点b) — the gateway
// clone URL (dispatcher.buildGatewayCloneURL's "<gatewayURL>/j/<token>/..."
// form) a writable job's own in-sandbox `git push` needs, since a plain
// local clone from bareRepoPath would otherwise leave `remote.origin.url`
// pointing at a DAEMON-filesystem path meaningless (and unreachable) from
// inside the job container. Empty skips this step, leaving origin pointed
// at bareRepoPath — the correct behavior for a read-only/no-gateway
// dispatch (nothing inside the sandbox will ever `git push` in that case).
// remoteURL carries a live per-job gateway token exactly like
// sandbox.CloneSpec.URL does; any error this function returns has already
// been redacted (via runGit -> redactGitArgs, which also strips this
// specific /j/<token>/ shape — see bare_repo.go's
// gatewayJobTokenPathPattern).
//
// On any failure ONCE stagingDir has actually started being populated (i.e.
// after the pre-clone RemoveAll below), stagingDir is removed before
// returning so a caller never inherits a half-populated staging area
// (codex round-1, PR834 Major 1: this previously only happened on the
// checkout/remote-url failure paths, not on `git clone` itself failing —
// the deferred cleanup below now covers every failure path uniformly,
// including clone failure, so a caller — runner.go's own trackCheckoutDir,
// which only starts tracking stagingDir on full success — can never be
// left responsible for cleaning up a staging dir it never learned about).
// PrepareJobCheckoutInput bundles PrepareJobCheckout's parameters
// (docs/plans/refactoring-backlog.md N10): the pre-struct signature had six
// consecutive string arguments, with RemoteURL (a per-job gateway URL
// carrying a live credential token) directly adjacent to
// BaseBranchForkPoint — a transposition there would silently feed the
// credentialed URL to git as a fork-point ref, and vice versa, with no
// compiler error to catch it.
type PrepareJobCheckoutInput struct {
	BareRepoPath        string
	Branch              string
	BaseBranch          string
	BaseBranchForkPoint string
	RemoteURL           string
	StagingDir          string
}

func PrepareJobCheckout(ctx context.Context, in PrepareJobCheckoutInput) (err error) {
	bareRepoPath := in.BareRepoPath
	branch := in.Branch
	baseBranch := in.BaseBranch
	baseBranchForkPoint := in.BaseBranchForkPoint
	remoteURL := in.RemoteURL
	stagingDir := in.StagingDir
	if bareRepoPath == "" || branch == "" || baseBranch == "" || stagingDir == "" {
		return fmt.Errorf("prepare job checkout: bare_repo_path/branch/base_branch/staging_dir must all be set (bare_repo_path=%q branch=%q base_branch=%q staging_dir=%q)",
			bareRepoPath, branch, baseBranch, stagingDir)
	}

	if mkErr := os.MkdirAll(filepath.Dir(stagingDir), 0o755); mkErr != nil {
		return fmt.Errorf("prepare job checkout: create staging parent dir: %w", mkErr)
	}
	// Reopen: idempotent by re-clone (see doc comment above) — wipe any
	// leftover stagingDir from a previous attempt before cloning fresh.
	if rmErr := os.RemoveAll(stagingDir); rmErr != nil {
		return fmt.Errorf("prepare job checkout: clear stale staging dir: %w", rmErr)
	}

	// From this point on, a failure may leave stagingDir partially
	// populated (a clone half-copied, a checkout that ran but a subsequent
	// remote set-url that didn't, ...) — clean it up unconditionally on any
	// non-nil return so the caller never has to. `return fmt.Errorf(...)`
	// below still assigns the named `err` result before this defer runs,
	// even though each step's own error is captured in a step-local
	// variable first.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stagingDir)
		}
	}()

	cloneArgs := []string{"clone", "--", "file://" + bareRepoPath, stagingDir}
	if cloneErr := runGit(exec.CommandContext(ctx, "git", cloneArgs...)); cloneErr != nil {
		return fmt.Errorf("prepare job checkout: clone from bare repo: %w", cloneErr)
	}

	run := func(args ...string) error {
		return runGit(exec.CommandContext(ctx, "git", append([]string{"-C", stagingDir}, args...)...))
	}
	if _, resolveErr := sandbox.ResolveCloneBranchRef(run, branch, baseBranch, baseBranchForkPoint); resolveErr != nil {
		return fmt.Errorf("prepare job checkout: %w", resolveErr)
	}

	if remoteURL != "" {
		remoteArgs := []string{"-C", stagingDir, "remote", "set-url", "origin", remoteURL}
		if remErr := runGit(exec.CommandContext(ctx, "git", remoteArgs...)); remErr != nil {
			return fmt.Errorf("prepare job checkout: set remote origin url: %w", remErr)
		}
	}

	return nil
}

// CleanupJobCheckout removes stagingDir (docs/plans/volume-only-daemon.md
// §論点b step 5: "job 終了時、 staging area を削除") — the bare repo cache at
// bareRepoPath (never touched by this function) remains untouched;
// stagingDir's own `.git/objects` is a full, standalone copy of what it
// needs (PrepareJobCheckout's own doc comment — a plain `file://` clone,
// not an alternates reference), so removing stagingDir never risks the bare
// repo's own object store either way.
//
// A no-op (nil error) for an empty stagingDir — the same "nothing to do"
// convention os.RemoveAll itself already has for a path that doesn't exist,
// made explicit here so a caller that never successfully ran
// PrepareJobCheckout (e.g. dispatch failed before staging was ever
// attempted) can unconditionally defer this without a nil/empty check of
// its own.
func CleanupJobCheckout(stagingDir string) error {
	if stagingDir == "" {
		return nil
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("cleanup job checkout: remove staging dir %s: %w", stagingDir, err)
	}
	return nil
}
