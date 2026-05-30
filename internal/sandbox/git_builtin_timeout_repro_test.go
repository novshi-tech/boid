package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// installHangingGit points realGitBinary() at a fake `git` that never returns
// in a useful time, standing in for a hung host-side git subprocess (e.g. a
// network op whose TCP connect stalls — GIT_TERMINAL_PROMPT=0 stops the auth
// prompt but nothing else bounds it).
func installHangingGit(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := realGitPath
	realGitPath = fake
	t.Cleanup(func() { realGitPath = prev })
}

// TestDirectGit_TimesOutInsteadOfHanging guards the executor-side fix.
//
// `git rev-parse`/`status` run host-side via execDirectGit. A hung git used to
// block it forever (cmd.Run() with no deadline); the agent's Bash tool timed
// out and recorded an empty result. execDirectGit must now kill git at the
// deadline and return a non-zero exit with an explicit message.
func TestDirectGit_TimesOutInsteadOfHanging(t *testing.T) {
	installHangingGit(t)
	prev := gitDirectTimeout
	gitDirectTimeout = 300 * time.Millisecond
	t.Cleanup(func() { gitDirectTimeout = prev })

	ch := make(chan *ExecResponse, 1)
	go func() { ch <- execDirectGit(t.TempDir(), []string{"rev-parse", "HEAD"}) }()

	select {
	case resp := <-ch:
		if resp.ExitCode == 0 {
			t.Fatalf("expected non-zero exit on timeout, got 0 (stderr=%q)", resp.Stderr)
		}
		if !strings.Contains(resp.Stderr, "timed out") {
			t.Fatalf("expected a timeout message in stderr, got %q", resp.Stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execDirectGit did not return well after its 300ms deadline — still hanging")
	}
}

// TestGitBuiltin_NoDeadlockAcrossHang guards the release-path fix.
//
// Two fetches on the same worktree previously serialized on a per-worktree
// mutex; a hung first fetch held it forever and the second blocked indefinitely,
// so every later fetch/push for that worktree returned empty. With the lock
// removed (git's own locking is sufficient) and a deadline on the run, both
// calls return.
func TestGitBuiltin_NoDeadlockAcrossHang(t *testing.T) {
	installHangingGit(t)
	prev := gitNetworkTimeout
	gitNetworkTimeout = 300 * time.Millisecond
	t.Cleanup(func() { gitNetworkTimeout = prev })

	binding := &GitBinding{
		WorktreeRoot: t.TempDir(),
		Remotes:      map[string]GitRemote{"origin": {FetchURL: "https://example.invalid/repo.git"}},
	}
	req := &GitRequest{
		Op:       GitOpFetch,
		Remote:   "origin",
		Refspecs: []string{"refs/heads/main:refs/remotes/origin/main"},
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); execGitBuiltin(req, binding) }()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		// Both returned — no deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent execGitBuiltin did not both return — a per-worktree lock cascade is back")
	}
}
