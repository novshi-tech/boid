package dispatcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// path dependency: TargetDir's own .git is fully self-contained (modulo
// --reference, which only ever needs to resolve from the DAEMON's own
// process — the job container never runs git against it).
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
// `--reference bareRepoPath` (alongside cloning directly FROM bareRepoPath)
// is not strictly load-bearing for object sharing — a same-filesystem local
// clone already auto-hardlinks objects with its source — but makes the
// intended object-sharing relationship explicit rather than relying on
// git's own local-clone heuristic, matching the plan doc's own "git clone
// --reference で cache 参照" wording. Because stagingDir's `.git/objects`
// ends up as an alternates reference into bareRepoPath rather than a full
// copy, stagingDir must never outlive bareRepoPath (it does not, by
// contract — see CleanupJobCheckout).
//
// `git checkout -B <branch> origin/<branch>` (not a plain `git checkout`)
// mirrors internal/sandbox/runner/clone.go's resolveCloneBranch: forces the
// local branch to exactly match the bare repo's own ref, discarding any
// local drift a same-named branch might otherwise carry from `git clone`'s
// own default-branch checkout — the identical idempotent-by-reclone
// contract the in-sandbox clone sequence already established (docs/plans/
// branch-policy-simplification.md: reopen = re-running this exact
// sequence).
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
// On any failure, stagingDir is removed before returning so a caller never
// inherits a half-populated staging area.
func PrepareJobCheckout(ctx context.Context, bareRepoPath, branch, remoteURL, stagingDir string) error {
	if bareRepoPath == "" || branch == "" || stagingDir == "" {
		return fmt.Errorf("prepare job checkout: bare_repo_path/branch/staging_dir must all be set (bare_repo_path=%q branch=%q staging_dir=%q)",
			bareRepoPath, branch, stagingDir)
	}

	if err := os.MkdirAll(filepath.Dir(stagingDir), 0o755); err != nil {
		return fmt.Errorf("prepare job checkout: create staging parent dir: %w", err)
	}
	// Reopen: idempotent by re-clone (see doc comment above) — wipe any
	// leftover stagingDir from a previous attempt before cloning fresh.
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("prepare job checkout: clear stale staging dir: %w", err)
	}

	cloneArgs := []string{"clone", "--reference", bareRepoPath, "--", bareRepoPath, stagingDir}
	if err := runGit(exec.CommandContext(ctx, "git", cloneArgs...)); err != nil {
		return fmt.Errorf("prepare job checkout: clone from bare repo: %w", err)
	}

	checkoutArgs := []string{"-C", stagingDir, "checkout", "-B", branch, "origin/" + branch}
	if err := runGit(exec.CommandContext(ctx, "git", checkoutArgs...)); err != nil {
		_ = os.RemoveAll(stagingDir)
		return fmt.Errorf("prepare job checkout: checkout branch %q: %w", branch, err)
	}

	if remoteURL != "" {
		remoteArgs := []string{"-C", stagingDir, "remote", "set-url", "origin", remoteURL}
		if err := runGit(exec.CommandContext(ctx, "git", remoteArgs...)); err != nil {
			_ = os.RemoveAll(stagingDir)
			return fmt.Errorf("prepare job checkout: set remote origin url: %w", err)
		}
	}

	return nil
}

// CleanupJobCheckout removes stagingDir (docs/plans/volume-only-daemon.md
// §論点b step 5: "job 終了時、 staging area を削除") — the bare repo cache at
// bareRepoPath (never touched by this function) remains untouched;
// stagingDir's own `.git/objects` is only ever an alternates REFERENCE into
// it (PrepareJobCheckout's own doc comment), never a copy, so removing
// stagingDir never risks the bare repo's own object store.
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
