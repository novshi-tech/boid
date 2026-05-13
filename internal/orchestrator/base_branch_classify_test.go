package orchestrator

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const classifyGitBin = "/usr/bin/git"

// initClassifyRepo creates a fresh git repo on the given branch with a single
// initial commit. Tests can then mutate the repo (extra commits, checkouts,
// remotes) without each setup having to repeat the boilerplate.
func initClassifyRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"symbolic-ref", "HEAD", "refs/heads/" + branch},
	} {
		cmd := exec.Command(classifyGitBin, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(out), "cwd does not exist") {
				t.Skip("git not available in this environment")
			}
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("classify test"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command(classifyGitBin, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// runGit fails the test if the command errors. Keeps individual cases
// readable.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(classifyGitBin, append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestClassifyBaseBranch_Case1_HeadMatches(t *testing.T) {
	dir := initClassifyRepo(t, "main")

	state, err := ClassifyBaseBranch(dir, "main")
	if err != nil {
		t.Fatalf("ClassifyBaseBranch: %v", err)
	}
	if state != Case1HeadMatches {
		t.Errorf("state = %v, want Case1HeadMatches", state)
	}
}

func TestClassifyBaseBranch_Case1_EmptyBaseBranchDefaultsToMain(t *testing.T) {
	dir := initClassifyRepo(t, "main")

	state, err := ClassifyBaseBranch(dir, "")
	if err != nil {
		t.Fatalf("ClassifyBaseBranch: %v", err)
	}
	if state != Case1HeadMatches {
		t.Errorf("state = %v, want Case1HeadMatches (empty defaults to main)", state)
	}
}

func TestClassifyBaseBranch_Case2_LocalBranchExists(t *testing.T) {
	dir := initClassifyRepo(t, "main")
	// Create a second branch but leave HEAD on main.
	runGit(t, dir, "branch", "develop")

	state, err := ClassifyBaseBranch(dir, "develop")
	if err != nil {
		t.Fatalf("ClassifyBaseBranch: %v", err)
	}
	if state != Case2ExistsButNotCheckedOut {
		t.Errorf("state = %v, want Case2ExistsButNotCheckedOut", state)
	}
}

func TestClassifyBaseBranch_Case2_RemoteOnlyBranch(t *testing.T) {
	// Set up a remote and a local repo where origin/feature exists but
	// the local repo has no "feature" branch.
	remote := initClassifyRepo(t, "main")
	runGit(t, remote, "checkout", "-q", "-b", "feature")
	runGit(t, remote, "commit", "-q", "--allow-empty", "-m", "feature commit")
	runGit(t, remote, "checkout", "-q", "main")

	local := t.TempDir()
	runGit(t, local, "init", "-q")
	runGit(t, local, "config", "user.email", "test@test.com")
	runGit(t, local, "config", "user.name", "Test")
	runGit(t, local, "remote", "add", "origin", remote)
	runGit(t, local, "fetch", "-q", "origin")
	runGit(t, local, "checkout", "-q", "-b", "main", "origin/main")

	state, err := ClassifyBaseBranch(local, "feature")
	if err != nil {
		t.Fatalf("ClassifyBaseBranch: %v", err)
	}
	if state != Case2ExistsButNotCheckedOut {
		t.Errorf("state = %v, want Case2ExistsButNotCheckedOut (origin/feature exists)", state)
	}
}

func TestClassifyBaseBranch_Case3_NotFound(t *testing.T) {
	dir := initClassifyRepo(t, "main")

	state, err := ClassifyBaseBranch(dir, "nope-not-a-real-branch")
	if err != nil {
		t.Fatalf("ClassifyBaseBranch: %v", err)
	}
	if state != Case3NotFound {
		t.Errorf("state = %v, want Case3NotFound", state)
	}
}

func TestClassifyBaseBranch_DetachedHead_ReturnsError(t *testing.T) {
	dir := initClassifyRepo(t, "main")

	out, err := exec.Command(classifyGitBin, "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	hash := strings.TrimSpace(string(out))
	runGit(t, dir, "checkout", "-q", "--detach", hash)

	state, err := ClassifyBaseBranch(dir, "main")
	if err == nil {
		t.Fatalf("expected error for detached HEAD, got state=%v", state)
	}
	if !errors.Is(err, ErrDetachedHead) {
		t.Errorf("error = %v, want errors.Is(ErrDetachedHead)", err)
	}
}

func TestClassifyBaseBranch_OriginPrefixStripped(t *testing.T) {
	// "origin/main" should be treated equivalently to "main" for the HEAD
	// match: project on local main with origin/main both existing → case 1.
	remote := initClassifyRepo(t, "main")
	local := t.TempDir()
	runGit(t, local, "init", "-q")
	runGit(t, local, "config", "user.email", "test@test.com")
	runGit(t, local, "config", "user.name", "Test")
	runGit(t, local, "remote", "add", "origin", remote)
	runGit(t, local, "fetch", "-q", "origin")
	runGit(t, local, "checkout", "-q", "-b", "main", "origin/main")

	state, err := ClassifyBaseBranch(local, "origin/main")
	if err != nil {
		t.Fatalf("ClassifyBaseBranch: %v", err)
	}
	if state != Case1HeadMatches {
		t.Errorf("state = %v, want Case1HeadMatches (origin/main stripped to main matches HEAD)", state)
	}
}

func TestClassifyBaseBranch_EmptyProjectDir_ReturnsError(t *testing.T) {
	_, err := ClassifyBaseBranch("", "main")
	if err == nil {
		t.Fatal("expected error for empty projectDir")
	}
}

func TestClassifyBaseBranch_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := ClassifyBaseBranch(dir, "main")
	if err == nil {
		t.Fatal("expected error for non-git directory")
	}
}
