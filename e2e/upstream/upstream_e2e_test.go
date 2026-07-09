//go:build e2e

// This file drives a real git binary against a real repository (clone,
// commit, push, fetch) over the fixture HTTP server, so — like
// internal/sandbox/git_builtin_test.go — it is excluded from plain `go test
// ./...` and run explicitly in CI via `go test -tags=e2e
// ./internal/sandbox/... ./e2e/upstream/...`.
package upstream_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/e2e/upstream"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@boid.test",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@boid.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// TestServeClonePush exercises the full lifecycle a scenario harness relies
// on: create a bare repo, push a real commit to it from a source
// worktree, then clone it into a fresh directory and confirm the content
// round-trips. This is the scenario described in
// docs/plans/git-gateway-cutover.md PR7a: e2e project directories get a
// real, reachable origin.
func TestServeClonePush(t *testing.T) {
	u := upstream.Serve(t, upstream.Options{})

	if _, err := u.NewRepo("app"); err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	// Idempotent: creating the same repo twice must not fail or reset it.
	if _, err := u.NewRepo("app"); err != nil {
		t.Fatalf("NewRepo (second call): %v", err)
	}

	src := t.TempDir()
	runGit(t, src, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello e2e upstream\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-q", "-m", "initial commit")
	runGit(t, src, "remote", "add", "origin", u.URL("app"))
	runGit(t, src, "push", "-q", "-u", "origin", "main")

	clone := t.TempDir()
	cloneDest := filepath.Join(clone, "app")
	runGit(t, clone, "clone", "-q", u.URL("app"), cloneDest)

	got, err := os.ReadFile(filepath.Join(cloneDest, "hello.txt"))
	if err != nil {
		t.Fatalf("read cloned file: %v", err)
	}
	if string(got) != "hello e2e upstream\n" {
		t.Fatalf("cloned file content = %q, want %q", got, "hello e2e upstream\n")
	}
}

// TestURLUnknownRepo404s confirms the fixture server behaves like a real
// git-http-backend for a repo that was never created via NewRepo: git
// itself reports the failure (rather than the server hanging or the
// process crashing), giving scenario authors a clear signal if a harness
// bug ever pushes to a name it never registered.
func TestURLUnknownRepo404s(t *testing.T) {
	u := upstream.Serve(t, upstream.Options{})

	dst := t.TempDir()
	cmd := exec.Command("git", "clone", "-q", u.URL("does-not-exist"), filepath.Join(dst, "x"))
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected clone of an unregistered repo to fail, got success: %s", out)
	}
}
