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

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "", stagingDir); err != nil {
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

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "feature", "", stagingDir); err != nil {
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

// TestPrepareJobCheckout_ReferenceSharesObjects pins that stagingDir's
// object store is an alternates REFERENCE into bareRepoPath, not a full
// copy (PrepareJobCheckout's own doc comment: "stagingDir must never
// outlive bareRepoPath") — the concrete, observable signal is an
// objects/info/alternates file inside stagingDir's .git pointing back at
// bareRepoPath's own objects dir.
func TestPrepareJobCheckout_ReferenceSharesObjects(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-3", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	alternates, err := os.ReadFile(filepath.Join(stagingDir, ".git", "objects", "info", "alternates"))
	if err != nil {
		t.Fatalf("read objects/info/alternates: %v", err)
	}
	if !strings.Contains(string(alternates), bareRepoPath) {
		t.Fatalf("alternates = %q, want it to reference bare repo path %q", alternates, bareRepoPath)
	}
}

func TestPrepareJobCheckout_ReopenWipesStaleContent(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-4", "proj")

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout (first): %v", err)
	}
	stray := filepath.Join(stagingDir, "stray-agent-output.txt")
	if err := os.WriteFile(stray, []byte("leftover from a previous job run\n"), 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	// Reopen: idempotent by re-clone, mirroring the in-sandbox clone
	// sequence's own reopen contract.
	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "", stagingDir); err != nil {
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

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", gatewayURL, stagingDir); err != nil {
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

	if err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "main", "", stagingDir); err != nil {
		t.Fatalf("PrepareJobCheckout: %v", err)
	}

	got := remoteOriginURL(t, stagingDir)
	if got != bareRepoPath {
		t.Fatalf("remote.origin.url = %q, want bare repo path %q (unchanged, no remoteURL given)", got, bareRepoPath)
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
		stagingDir   string
	}{
		{"empty bare repo path", "", "main", "/tmp/staging"},
		{"empty branch", "/tmp/bare.git", "", "/tmp/staging"},
		{"empty staging dir", "/tmp/bare.git", "main", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := dispatcher.PrepareJobCheckout(context.Background(), tc.bareRepoPath, tc.branch, "", tc.stagingDir)
			if err == nil {
				t.Fatal("expected an error for missing required argument, got nil")
			}
		})
	}
}

func TestPrepareJobCheckout_InvalidBranchCleansUpStagingDir(t *testing.T) {
	bareRepoPath := setupBareRepoFixture(t)
	stagingDir := filepath.Join(t.TempDir(), "staging", "job-7", "proj")

	err := dispatcher.PrepareJobCheckout(context.Background(), bareRepoPath, "does-not-exist-branch", "", stagingDir)
	if err == nil {
		t.Fatal("expected an error for a branch that does not exist in the bare repo")
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
