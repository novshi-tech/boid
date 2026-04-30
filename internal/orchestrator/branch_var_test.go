package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const branchVarGitBin = "/usr/bin/git"

func initBranchVarRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"symbolic-ref", "HEAD", "refs/heads/" + branch},
	} {
		cmd := exec.Command(branchVarGitBin, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(out), "cwd does not exist") {
				t.Skip("git not available in this environment")
			}
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f := filepath.Join(dir, "README")
	os.WriteFile(f, []byte("test"), 0o644)
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command(branchVarGitBin, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(out), "cwd does not exist") {
				t.Skip("git not available in this environment")
			}
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestExpandBaseBranch_NoVariable(t *testing.T) {
	got, err := ExpandBaseBranch("main", "/irrelevant")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "main" {
		t.Errorf("got %q, want %q", got, "main")
	}
}

func TestExpandBaseBranch_BracedVariable(t *testing.T) {
	dir := initBranchVarRepo(t, "feature/my-branch")

	got, err := ExpandBaseBranch("${current_branch}", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "feature/my-branch" {
		t.Errorf("got %q, want %q", got, "feature/my-branch")
	}
}

func TestExpandBaseBranch_UnbracedVariable(t *testing.T) {
	dir := initBranchVarRepo(t, "my-branch")

	got, err := ExpandBaseBranch("$current_branch", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "my-branch" {
		t.Errorf("got %q, want %q", got, "my-branch")
	}
}

func TestExpandBaseBranch_DetachedHead(t *testing.T) {
	dir := initBranchVarRepo(t, "main")

	cmd := exec.Command(branchVarGitBin, "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	hash := strings.TrimSpace(string(out))

	detachCmd := exec.Command(branchVarGitBin, "checkout", "--detach", hash)
	detachCmd.Dir = dir
	if out, err := detachCmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout --detach: %v\n%s", err, out)
	}

	_, err = ExpandBaseBranch("${current_branch}", dir)
	if err == nil {
		t.Fatal("expected error for detached HEAD, got nil")
	}
}

func TestExpandBaseBranch_UnknownVariable(t *testing.T) {
	got, err := ExpandBaseBranch("${unknown}", "/irrelevant")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "${unknown}" {
		t.Errorf("got %q, want %q", got, "${unknown}")
	}
}

func TestExpandBaseBranch_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := ExpandBaseBranch("${current_branch}", dir)
	if err == nil {
		t.Fatal("expected error for non-git directory, got nil")
	}
}
