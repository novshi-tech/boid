package testutil

import (
	"os/exec"
	"testing"
)

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
