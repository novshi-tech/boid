package testutil

import (
	"os/exec"
	"testing"
)

// InitGitRepoWithOrigin git-inits dir, makes an initial (empty) commit, and
// adds an origin remote so it satisfies `project add`'s upstream_url capture
// requirement. The origin URL is a fixed placeholder — tests exercising this
// helper only need `project add` / `project reload` to see *some* origin,
// never a real fetchable remote.
//
// The initial commit exists so `git rev-parse --abbrev-ref HEAD` (used by
// orchestrator.ExpandBaseBranch's ${current_branch} expansion) resolves: a
// freshly `git init`'d repo with zero commits has an unborn HEAD, which
// rev-parse rejects with exit 128 ("ambiguous argument 'HEAD'"). -c
// user.name/user.email are passed explicitly rather than relying on ambient
// git config, which a bare CI runner may not have.
func InitGitRepoWithOrigin(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "-c", "user.name=testutil", "-c", "user.email=testutil@boid.test",
		"commit", "--allow-empty", "-q", "-m", "testutil: initial commit")
	runGit(t, dir, "remote", "add", "origin", "https://example.invalid/testutil/repo.git")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
}
