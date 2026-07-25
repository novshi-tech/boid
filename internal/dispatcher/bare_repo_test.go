package dispatcher_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
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

// gitCredentialFixtureServer starts an httptest.Server standing in for a
// forge host, recording the Authorization header of every request it
// receives (so a test can prove credentialGitArgs' `-c http.extraHeader=...`
// actually reached an outbound git request) before always responding 404 —
// enough for git's smart-HTTP discovery (`GET .../info/refs?service=...`) to
// fail cleanly and quickly, without needing a real git-http-backend.
func gitCredentialFixtureServer(t *testing.T) (url string, lastAuth func() string) {
	t.Helper()
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("Authorization")
		mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() string {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// TestCloneBareRepo_DoesNotPersistCredentialsInURL pins two things at once
// against a real HTTP fixture whose host credentialGitArgs actually
// recognizes (Minor 3, codex round-1 review: the pre-fix version pointed
// creds at "github.com" while cloning from a bare local filesystem path,
// whose host can never be determined — credentialGitArgs took its "can't
// determine host" no-op branch, so the test never exercised
// `http.extraHeader` at all):
//
//  1. the Authorization header the fixture server observed proves the
//     credential really was injected via `-c http.extraHeader=...`
//     (positive proof the mechanism engaged, not just "clone succeeded
//     unauthenticated");
//  2. the returned error from the resulting (expected) clone failure does
//     not contain the raw token or its base64-encoded Basic-auth form
//     (Blocker 1: runGit used to join cmd.Args — including that
//     `http.extraHeader=Authorization: Basic <token>` argument — verbatim
//     into the error CreateProjectFromGitURL/FetchProject surface to the
//     CLI/API caller and, for FetchProject, persist as StatusMessage).
func TestCloneBareRepo_DoesNotPersistCredentialsInURL(t *testing.T) {
	srvURL, lastAuth := gitCredentialFixtureServer(t)
	host := strings.TrimPrefix(srvURL, "http://")

	const rawToken = "s3cr3t-pat-token"
	resolver := func(namespace, key string) (string, error) { return rawToken, nil }
	creds := gitgateway.NewCredentialProvider([]gitgateway.HostForgeConfig{
		{Host: host, Forge: gitgateway.ForgeGitHub, SecretKey: "github-pat"},
	}, resolver)

	dest := filepath.Join(t.TempDir(), "dest.git")
	err := dispatcher.CloneBareRepo(context.Background(), srvURL+"/owner/repo.git", dest, creds, "default")
	if err == nil {
		t.Fatal("expected clone to fail against a non-git HTTP fixture (404 on info/refs)")
	}

	auth := lastAuth()
	if auth == "" {
		t.Fatal("fixture server never saw an Authorization header — credentialGitArgs did not engage http.extraHeader")
	}
	wantB64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + rawToken))
	if auth != "Basic "+wantB64 {
		t.Fatalf("Authorization header = %q, want %q", auth, "Basic "+wantB64)
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, rawToken) {
		t.Errorf("CloneBareRepo error leaks the raw credential token: %s", errMsg)
	}
	if strings.Contains(errMsg, wantB64) {
		t.Errorf("CloneBareRepo error leaks the base64-encoded Basic-auth header: %s", errMsg)
	}
}

// TestFetchBareRepo_ErrorDoesNotLeakCredentials is FetchBareRepo's
// counterpart to the clone-side test above — the same Blocker 1 leak path
// exists independently on the fetch side (dispatcher.FetchBareRepo ->
// credentialGitArgs -> runGit), and FetchProject additionally persists a
// fetch failure's message as the project's StatusMessage (visible via
// `boid project list`/`show` from then on), so pinning it here too matters.
func TestFetchBareRepo_ErrorDoesNotLeakCredentials(t *testing.T) {
	src := setupSourceRepo(t)
	dest := filepath.Join(t.TempDir(), "dest.git")
	if err := dispatcher.CloneBareRepo(context.Background(), src, dest, nil, "default"); err != nil {
		t.Fatalf("CloneBareRepo: %v", err)
	}

	srvURL, lastAuth := gitCredentialFixtureServer(t)
	host := strings.TrimPrefix(srvURL, "http://")
	runGitT(t, dest, "remote", "set-url", "origin", srvURL+"/owner/repo.git")

	const rawToken = "another-s3cr3t-token"
	resolver := func(namespace, key string) (string, error) { return rawToken, nil }
	creds := gitgateway.NewCredentialProvider([]gitgateway.HostForgeConfig{
		{Host: host, Forge: gitgateway.ForgeGitHub, SecretKey: "github-pat"},
	}, resolver)

	err := dispatcher.FetchBareRepo(context.Background(), dest, creds, "default")
	if err == nil {
		t.Fatal("expected fetch to fail against a non-git HTTP fixture (404 on info/refs)")
	}

	if lastAuth() == "" {
		t.Fatal("fixture server never saw an Authorization header — credentialGitArgs did not engage http.extraHeader")
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, rawToken) {
		t.Errorf("FetchBareRepo error leaks the raw credential token: %s", errMsg)
	}
	wantB64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + rawToken))
	if strings.Contains(errMsg, wantB64) {
		t.Errorf("FetchBareRepo error leaks the base64-encoded Basic-auth header: %s", errMsg)
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

// TestFetchBareRepo_RefreshesHEADAfterRemoteDefaultBranchChange pins MAJOR 4
// (PR-2a codex round-1 review): a --mirror clone's fetch refspec updates
// refs/heads/* in place but never touches HEAD on its own, so a forge that
// renames its default branch after the project was registered must not
// permanently strand the project reading the old (and, once `--prune`
// removes it, nonexistent) branch.
func TestFetchBareRepo_RefreshesHEADAfterRemoteDefaultBranchChange(t *testing.T) {
	src := setupSourceRepo(t)
	dest := filepath.Join(t.TempDir(), "dest.git")
	if err := dispatcher.CloneBareRepo(context.Background(), src, dest, nil, "default"); err != nil {
		t.Fatalf("CloneBareRepo: %v", err)
	}
	if got, err := orchestrator.DefaultBranch(dest); err != nil || got != "main" {
		t.Fatalf("precondition: HEAD = %q, err = %v, want main", got, err)
	}

	// Rename the source's default branch — simulating a forge's "main"
	// (deprecated) -> "trunk" default-branch change — then fetch with
	// --prune, which removes the now-gone "main" local ref entirely.
	runGitT(t, src, "branch", "-m", "main", "trunk")

	if err := dispatcher.FetchBareRepo(context.Background(), dest, nil, "default"); err != nil {
		t.Fatalf("FetchBareRepo: %v", err)
	}

	got, err := orchestrator.DefaultBranch(dest)
	if err != nil {
		t.Fatalf("HEAD after fetch is unreadable (expected it refreshed to trunk): %v", err)
	}
	if got != "trunk" {
		t.Errorf("HEAD after fetch = %q, want trunk (HEAD was not refreshed to the remote's new default branch)", got)
	}
}

func TestCloneBareRepo_InvalidSourceFails(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dest.git")
	err := dispatcher.CloneBareRepo(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), dest, nil, "default")
	if err == nil {
		t.Fatal("expected error cloning a nonexistent source")
	}
}
