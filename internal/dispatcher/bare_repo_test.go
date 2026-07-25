package dispatcher_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
)

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setupSourceRepo builds a plain (non-bare) local git repo with a single
// commit, usable as a `git clone`-able source URL (a local filesystem path
// is a perfectly valid git URL — no network/credentials involved, letting
// these tests exercise the real CloneBareRepo/FetchBareRepo code paths
// end-to-end without a fake forge).
func setupSourceRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitT(t, dir, "init", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "test@example.com")
	runGitT(t, dir, "config", "user.name", "Test")
	runGitT(t, dir, "add", ".")
	runGitT(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func TestCloneBareRepo_LocalSource(t *testing.T) {
	src := setupSourceRepo(t)
	dest := filepath.Join(t.TempDir(), "dest.git")

	// No credentials configured at all — creds is nil, exercising the
	// "proceed unauthenticated" path (correct for a local/public source).
	if err := dispatcher.CloneBareRepo(context.Background(), src, dest, nil, "default"); err != nil {
		t.Fatalf("CloneBareRepo: %v", err)
	}

	if fi, err := os.Stat(filepath.Join(dest, "HEAD")); err != nil || fi.IsDir() {
		t.Fatalf("expected a bare repo at %s (HEAD file missing): %v", dest, err)
	}
}

func TestCloneBareRepo_DoesNotPersistCredentialsInURL(t *testing.T) {
	src := setupSourceRepo(t)
	dest := filepath.Join(t.TempDir(), "dest.git")

	// Configure creds for a host that never matches "src" (a local path has
	// no recognizable host), so credentialGitArgs takes the "can't
	// determine host" no-op branch — clone still succeeds unauthenticated.
	resolver := func(namespace, key string) (string, error) { return "secret-token", nil }
	creds := gitgateway.NewCredentialProvider([]gitgateway.HostForgeConfig{
		{Host: "github.com", Forge: gitgateway.ForgeGitHub, SecretKey: "github-pat"},
	}, resolver)

	if err := dispatcher.CloneBareRepo(context.Background(), src, dest, creds, "default"); err != nil {
		t.Fatalf("CloneBareRepo: %v", err)
	}

	cmd := exec.Command("git", "-C", dest, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git remote get-url origin: %v", err)
	}
	got := string(out)
	if got == "" {
		t.Fatal("empty remote origin url")
	}
	// The stored origin URL must be exactly the plain source path — no
	// embedded Basic-auth credentials, confirming CloneBareRepo never wrote
	// a credentialed URL into the bare repo's own config.
	if got[:len(got)-1] != src {
		t.Errorf("remote.origin.url = %q, want %q (must not contain embedded credentials)", got, src)
	}
}

func TestFetchBareRepo_LocalSource(t *testing.T) {
	src := setupSourceRepo(t)
	dest := filepath.Join(t.TempDir(), "dest.git")
	if err := dispatcher.CloneBareRepo(context.Background(), src, dest, nil, "default"); err != nil {
		t.Fatalf("CloneBareRepo: %v", err)
	}

	// Add a new commit to the source, then fetch it into the bare repo.
	if err := os.WriteFile(filepath.Join(src, "second.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitT(t, src, "add", ".")
	runGitT(t, src, "commit", "-q", "-m", "second")

	if err := dispatcher.FetchBareRepo(context.Background(), dest, nil, "default"); err != nil {
		t.Fatalf("FetchBareRepo: %v", err)
	}

	// A --mirror clone's fetch refspec (+refs/*:refs/*) updates refs/heads/*
	// directly (unlike a plain --bare clone, which sets no refspec at all —
	// see CloneBareRepo's doc comment for why --mirror is required for this
	// to work) — so the new commit must now be reachable from refs/heads/main
	// itself, not from a refs/remotes/origin/main that a mirror never
	// creates.
	cmd := exec.Command("git", "-C", dest, "log", "--oneline", "refs/heads/main")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log refs/heads/main: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty log for refs/heads/main after fetch")
	}
	if !bytes.Contains(out, []byte("second")) {
		t.Errorf("expected the fetched 'second' commit in refs/heads/main log, got:\n%s", out)
	}
}

func TestCloneBareRepo_InvalidSourceFails(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dest.git")
	err := dispatcher.CloneBareRepo(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), dest, nil, "default")
	if err == nil {
		t.Fatal("expected error cloning a nonexistent source")
	}
}
