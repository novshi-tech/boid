package dispatcher_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
)

// setupBareRepoFixture builds a source repo with a commit on "main" and a
// second branch "feature", then mirror-clones it into a bare repo via
// dispatcher.CloneBareRepo (PR-2a's own primitive) — exercising the real
// daemon-managed bare-repo shape (§論点b layout) PrepareJobCheckout is
// meant to consume, rather than hand-rolling a bare repo some other way.
func setupBareRepoFixture(t *testing.T) (bareRepoPath string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	src := t.TempDir()
	runGitT(t, src, "init", "-q", "-b", "main")
	runGitT(t, src, "config", "user.email", "test@example.com")
	runGitT(t, src, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("main content\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitT(t, src, "add", ".")
	runGitT(t, src, "commit", "-q", "-m", "initial")
	runGitT(t, src, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(src, "feature.txt"), []byte("feature content\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitT(t, src, "add", ".")
	runGitT(t, src, "commit", "-q", "-m", "feature commit")
	runGitT(t, src, "checkout", "-q", "main")

	bareRepoPath = filepath.Join(t.TempDir(), "proj.git")
	if err := dispatcher.CloneBareRepo(context.Background(), src, bareRepoPath, nil, "default"); err != nil {
		t.Fatalf("CloneBareRepo fixture setup: %v", err)
	}
	return bareRepoPath
}

func TestPrepareJobCheckout_ClonesAndChecksOutBranch(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-1", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "main", "", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(stagingDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md from staging dir: %v", err)
	}
	if string(data) != "main content\n" {
		t.Fatalf("README.md content = %q, want %q", data, "main content\n")
	}
	if _, err := os.Stat(filepath.Join(stagingDir, ".git")); err != nil {
		t.Fatalf("staging dir has no .git: %v", err)
	}
}

func TestPrepareJobCheckout_ChecksOutNonDefaultBranch(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-2", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "feature", "feature", "", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	if _, err := os.Stat(filepath.Join(stagingDir, "feature.txt")); err != nil {
		t.Fatalf("expected feature.txt to exist on the feature branch checkout: %v", err)
	}

	branch := currentBranch(t, stagingDir)
	if branch != "feature" {
		t.Fatalf("checked-out branch = %q, want %q", branch, "feature")
	}
}

// TestPrepareJobCheckout_OriginPrefixedBaseBranchStripped pins the PR834
// PR-2b round-2 codex review Blocker: a project.yaml `base_branch:
// origin/main` (and, by CloneDeclaration's Branch==BaseBranch contract,
// `branch: origin/main` too) must resolve to a local branch literally named
// "main", checked out from origin/main — the pre-fix code instead built
// "origin/" + branch unconditionally, producing an unresolvable
// "origin/origin/main" and failing the whole dispatch.
func TestPrepareJobCheckout_OriginPrefixedBaseBranchStripped(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-origin-prefix", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "origin/main", "origin/main", "", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	branch := currentBranch(t, stagingDir)
	if branch != "main" {
		t.Fatalf("checked-out branch = %q, want %q (origin/ prefix stripped)", branch, "main")
	}
	data, err := os.ReadFile(filepath.Join(stagingDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md from staging dir: %v", err)
	}
	if string(data) != "main content\n" {
		t.Fatalf("README.md content = %q, want %q", data, "main content\n")
	}
}

// TestPrepareJobCheckout_MissingBaseBranchCreatedFromForkPoint pins the
// other PR834 PR-2b round-2 codex review Blocker gap: base_branch:
// release/1.0 with fork_point: main, where release/1.0 exists on neither
// origin nor locally yet, must create release/1.0 from the fork_point
// rather than failing the whole dispatch with "branch not found".
func TestPrepareJobCheckout_MissingBaseBranchCreatedFromForkPoint(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-fork-point", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "release/1.0", "release/1.0", "main", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	branch := currentBranch(t, stagingDir)
	if branch != "release/1.0" {
		t.Fatalf("checked-out branch = %q, want %q", branch, "release/1.0")
	}
	// release/1.0 was created from main's fork point, so it carries main's
	// content (not feature's).
	data, err := os.ReadFile(filepath.Join(stagingDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md from staging dir: %v", err)
	}
	if string(data) != "main content\n" {
		t.Fatalf("README.md content = %q, want %q (release/1.0 forked from main)", data, "main content\n")
	}
}

// TestPrepareJobCheckout_MissingBaseBranchFallsBackToOriginHEAD covers the
// no-fork_point sibling of the fork-point test above: a base branch missing
// from origin with no explicit fork_point is created from
// refs/remotes/origin/HEAD instead of erroring outright — matching
// internal/sandbox/runner/clone_e2e_test.go's own
// TestPerformClone_BaseBranchMissingFallsBackToOriginHEAD for the
// sandbox-internal path this shares its resolution algorithm with.
func TestPrepareJobCheckout_MissingBaseBranchFallsBackToOriginHEAD(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-origin-head-fallback", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "release/2.0", "release/2.0", "", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	branch := currentBranch(t, stagingDir)
	if branch != "release/2.0" {
		t.Fatalf("checked-out branch = %q, want %q", branch, "release/2.0")
	}
	// setupBareRepoFixture leaves the bare repo's default branch (origin/HEAD)
	// on "main" (main is checked out last), so release/2.0 carries main's
	// content.
	data, err := os.ReadFile(filepath.Join(stagingDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md from staging dir: %v", err)
	}
	if string(data) != "main content\n" {
		t.Fatalf("README.md content = %q, want %q (release/2.0 forked from origin/HEAD == main)", data, "main content\n")
	}
}

// TestPrepareJobCheckout_NoAlternatesDependency pins codex round-1 (PR834)
// Blocker 1's fix: stagingDir's object store must be a full, standalone
// copy, NOT an alternates REFERENCE into bareRepoPath — a job container
// only ever mounts stagingDir's own subtree, never the daemon's own
// bareRepoPath, so an alternates entry pointing back at bareRepoPath would
// be unresolvable the moment any in-sandbox git command (status/commit/
// log/...) needed to read an object through it. The concrete, observable
// signals: no objects/info/alternates file inside stagingDir's .git, and
// git operations against stagingDir still succeed standalone even after
// bareRepoPath is removed entirely (proving the object data was actually
// copied, not merely referenced/hardlinked).
func TestPrepareJobCheckout_NoAlternatesDependency(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-3", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "main", "", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	if _, err := os.Stat(filepath.Join(stagingDir, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("expected no objects/info/alternates file in stagingDir, stat err = %v", err)
	}

	// Remove the bare repo entirely (simulating it being invisible/unmounted
	// from a job container's own perspective) and verify stagingDir's git
	// operations still work standalone.
	if err := os.RemoveAll(bareRepoPath); err != nil {
		t.Fatalf("remove bare repo: %v", err)
	}
	runGitT(t, stagingDir, "status")
	runGitT(t, stagingDir, "log", "-1", "--oneline")
}

func TestPrepareJobCheckout_ReopenWipesStaleContent(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-4", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "main", "", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout (first): %v", err)
	}
	stray := filepath.Join(stagingDir, "stray-agent-output.txt")
	if err := os.WriteFile(stray, []byte("leftover from a previous job run\n"), 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	// Reopen: idempotent by re-clone, mirroring the in-sandbox clone
	// sequence's own reopen contract.
	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "main", "", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout (reopen): %v", err)
	}

	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("expected stray file to be wiped on reopen, stat err = %v", err)
	}
}

func TestPrepareJobCheckout_SetsRemoteURLWhenProvided(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-5", "proj")
	const gatewayURL = "http://10.0.2.2:12345/j/some-job-token/example.com/owner/repo.git"

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "main", "", gatewayURL, stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	got := remoteOriginURL(t, stagingDir)
	if got != gatewayURL {
		t.Fatalf("remote.origin.url = %q, want %q", got, gatewayURL)
	}
}

func TestPrepareJobCheckout_NoRemoteURLLeavesOriginAtBareRepo(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-6", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "main", "", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	// "file://" + bareRepoPath (codex round-1, PR834 Blocker 1's fix — the
	// clone itself now uses the file:// scheme, not a bare local path with
	// --reference), not the bare bareRepoPath value.
	want := "file://" + bareRepoPath
	got := remoteOriginURL(t, stagingDir)
	if got != want {
		t.Fatalf("remote.origin.url = %q, want %q (unchanged, no remoteURL given)", got, want)
	}
}

func TestPrepareJobCheckout_MissingArgsError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	cases := []struct {
		name         string
		bareRepoPath string
		branch       string
		baseBranch   string
		stagingDir   string
	}{
		{"empty bare repo path", "", "main", "main", "/tmp/staging"},
		{"empty branch", "/tmp/bare.git", "", "main", "/tmp/staging"},
		{"empty base branch", "/tmp/bare.git", "main", "", "/tmp/staging"},
		{"empty staging dir", "/tmp/bare.git", "main", "main", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := dispatcher.PrepareJobCheckout(context.Background(), tc.bareRepoPath, tc.branch, tc.baseBranch, "", "", tc.stagingDir)
			if err == nil {
				t.Fatal("expected an error for missing required argument, got nil")
			}
		})
	}
}

// TestPrepareJobCheckout_CloneFailureCleansUpStagingDir pins codex round-1
// (PR834) Major 1: a failure in the `git clone` step itself (not just the
// later checkout/remote-set-url steps, already covered by
// TestPrepareJobCheckout_InvalidBranchCleansUpStagingDir) must not leave an
// orphaned stagingDir behind — runner.go's own trackCheckoutDir only starts
// tracking stagingDir on FULL success, so a caller has no other way to find
// and clean up a staging dir abandoned mid-clone.
func TestPrepareJobCheckout_CloneFailureCleansUpStagingDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	bareRepoPath := filepath.Join(t.TempDir(), "does-not-exist.git")
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-9", "proj")

	err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "main", "", "", stagingDir)
	if err == nil {
		t.Fatal("expected an error cloning from a nonexistent bare repo path")
	}
	if _, statErr := os.Stat(stagingDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected staging dir to be cleaned up after a failed clone, stat err = %v", statErr)
	}
}

// TestPrepareJobCheckout_InvalidForkPointCleansUpStagingDir supersedes the
// pre-round-2 "a branch that does not exist in the bare repo errors"
// premise: as of the PR834 PR-2b round-2 fix, a base branch missing from
// origin is no longer itself an error — sandbox.ResolveCloneBranchRef
// creates it from baseBranchForkPoint (or, absent one, refs/remotes/
// origin/HEAD, which always resolves in a fresh non-empty clone) exactly
// like internal/sandbox/runner/clone.go's resolveCloneBranch already did
// (ClassifyBaseBranch case 3). A staging dir only ends up abandoned now
// when EVERY resolution path fails outright — an explicit fork_point that
// itself does not resolve in the clone.
func TestPrepareJobCheckout_InvalidForkPointCleansUpStagingDir(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-7", "proj")

	err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "does-not-exist-branch", "does-not-exist-branch", "does-not-exist-fork-point", "", stagingDir)
	if err == nil {
		t.Fatal("expected an error for a base branch whose fork_point does not resolve in the clone")
	}
	if _, statErr := os.Stat(stagingDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected staging dir to be cleaned up after a failed checkout, stat err = %v", statErr)
	}
}

func TestCleanupJobCheckout_RemovesStagingDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "staging", "job-8", "proj")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := dispatcher.CleanupJobCheckout(target); err != nil {
		t.Fatalf("CleanupJobCheckout: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected staging dir to be removed, stat err = %v", err)
	}
}

func TestCleanupJobCheckout_EmptyPathIsNoop(t *testing.T) {
	if err := dispatcher.CleanupJobCheckout(""); err != nil {
		t.Fatalf("CleanupJobCheckout(\"\") = %v, want nil", err)
	}
}

func TestCleanupJobCheckout_MissingDirIsNoop(t *testing.T) {
	if err := dispatcher.CleanupJobCheckout(filepath.Join(t.TempDir(), "never-existed")); err != nil {
		t.Fatalf("CleanupJobCheckout(missing dir) = %v, want nil", err)
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --abbrev-ref HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func remoteOriginURL(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git remote get-url origin: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}
