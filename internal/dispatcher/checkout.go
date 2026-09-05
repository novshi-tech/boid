package dispatcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// This file implements the daemon-side per-job clone: given a project's
// daemon-managed bare repository (see bare_repo.go's CloneBareRepo/
// FetchBareRepo), materialize a fresh, per-job working checkout ("staging
// area") the daemon can hand a job container as a real host-path bind mount.
//
// This deliberately does not use `git worktree`: a worktree's .git file
// records an absolute path back to the bare repo, which only resolves
// inside the same container/mount-namespace the bare repo lives in — a DooD
// sibling job container has no such path. A plain clone's .git is fully
// self-contained instead.
//
// This never talks to a remote forge, so it needs no credential provider:
// bareRepoPath is a local filesystem path (the daemon's own cache), not a
// credentialed URL. Callers must call FetchBareRepo first if they want the
// bare repo refreshed against its remote before staging off of it.

// PrepareJobCheckout materializes a fresh, per-job working checkout of
// branch at stagingDir, cloned from the daemon-managed bare repository at
// bareRepoPath.
//
// Cloning is a plain `git clone file://<bareRepoPath>`, not `git clone
// --reference`: `--reference` records bareRepoPath in stagingDir's own
// alternates file, which only resolves from a process that can see
// bareRepoPath on its own filesystem — true of the daemon, not of the job
// container the staging area gets bind-mounted into. The explicit `file://`
// scheme forces a real, standalone copy of every object stagingDir needs,
// so stagingDir never depends on bareRepoPath's continued existence or
// mount visibility (see TestPrepareJobCheckout_NoAlternatesDependency).
//
// Branch resolution shares sandbox.ResolveCloneBranchRef with
// internal/sandbox/runner/clone.go's resolveCloneBranch, so the two paths
// can't drift; see ResolveCloneBranchRef's own doc comment for the algorithm.
//
// remoteURL, when non-empty, is set as stagingDir's own `origin` remote
// after cloning — the gateway clone URL a writable job's own `git push`
// needs, since a plain local clone otherwise leaves `origin` pointing at a
// daemon-filesystem path unreachable from inside the job container. Empty
// skips this step (read-only / no-gateway dispatch never needs to push).
//
// A prior stagingDir is wiped before cloning fresh (reopen = re-running
// this exact sequence). On any failure once stagingDir has started being
// populated, it is removed before returning, so a caller never inherits a
// half-populated staging area.
func PrepareJobCheckout(ctx context.Context, bareRepoPath, branch, baseBranch, baseBranchForkPoint, remoteURL, stagingDir string) (err error) {
	if bareRepoPath == "" || branch == "" || baseBranch == "" || stagingDir == "" {
		return fmt.Errorf("prepare job checkout: bare_repo_path/branch/base_branch/staging_dir must all be set (bare_repo_path=%q branch=%q base_branch=%q staging_dir=%q)",
			bareRepoPath, branch, baseBranch, stagingDir)
	}

	if mkErr := os.MkdirAll(filepath.Dir(stagingDir), 0o755); mkErr != nil {
		return fmt.Errorf("prepare job checkout: create staging parent dir: %w", mkErr)
	}
	if rmErr := os.RemoveAll(stagingDir); rmErr != nil {
		return fmt.Errorf("prepare job checkout: clear stale staging dir: %w", rmErr)
	}

	// Clean up unconditionally on any non-nil return so the caller never
	// inherits a half-populated staging area.
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

// CleanupJobCheckout removes stagingDir. Safe to call unconditionally: the
// bare repo cache at bareRepoPath is never touched (stagingDir's objects are
// a standalone copy, not an alternates reference), and an empty stagingDir
// is a no-op so a caller that never reached PrepareJobCheckout can defer
// this without its own nil check.
func CleanupJobCheckout(stagingDir string) error {
	if stagingDir == "" {
		return nil
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("cleanup job checkout: remove staging dir %s: %w", stagingDir, err)
	}
	return nil
}
