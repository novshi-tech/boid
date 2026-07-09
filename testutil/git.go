package testutil

import (
	"os/exec"
	"testing"
)

// InitGitRepoWithOrigin git-inits dir and adds an origin remote so it
// satisfies `project add`'s upstream_url capture requirement (see
// docs/plans/git-gateway-cutover.md PR2: a project with no git origin remote
// is now rejected at registration time). The URL is a fixed placeholder —
// tests exercising this helper only need `project add` / `project reload` to
// see *some* origin, never a real fetchable remote.
func InitGitRepoWithOrigin(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "-q")
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
