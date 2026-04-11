package sandbox_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)


func gateGitPolicies() map[string]sandbox.BuiltinPolicy {
	return projectspec.DefaultBuiltinPolicies(projectspec.RoleGate, []string{"git"})
}

func hookGitPolicies() map[string]sandbox.BuiltinPolicy {
	return projectspec.DefaultBuiltinPolicies(projectspec.RoleHook, []string{"git"})
}

func TestBroker_GitBuiltinPushUsesTrustedSnapshot(t *testing.T) {
	repo := initGitRepo(t)
	remote1 := initBareRemote(t)
	remote2 := initBareRemote(t)

	runGit(t, repo, "remote", "add", "origin", remote1)
	runGit(t, repo, "push", "-u", "origin", "main")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, gateGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	runGit(t, repo, "remote", "set-url", "origin", remote2)
	writeFile(t, filepath.Join(repo, "README.md"), "mutated remote\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "second")

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Git: &sandbox.GitRequest{
			Op:     sandbox.GitOpPush,
			Remote: "origin",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("git push exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}

	localHead := runGit(t, repo, "rev-parse", "HEAD")
	remote1Head := runGitBare(t, remote1, "rev-parse", "refs/heads/main")
	if remote1Head != localHead {
		t.Fatalf("remote1 head = %q, want %q", remote1Head, localHead)
	}

	if cmd := exec.Command(realGitForTest, "--git-dir", remote2, "rev-parse", "--verify", "refs/heads/main"); cmd.Run() == nil {
		t.Fatal("remote2 should not have received the push")
	}
}

func TestBroker_GitBuiltinFetchWorks(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)

	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	peer := cloneRepo(t, remote)
	writeFile(t, filepath.Join(peer, "peer.txt"), "peer change\n")
	runGit(t, peer, "add", "peer.txt")
	runGit(t, peer, "commit", "-m", "peer")
	runGit(t, peer, "push", "origin", "main")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, gateGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Git: &sandbox.GitRequest{
			Op:     sandbox.GitOpFetch,
			Remote: "origin",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("git fetch exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}

	remoteHead := runGitBare(t, remote, "rev-parse", "refs/heads/main")
	fetchedHead := runGit(t, repo, "rev-parse", "refs/remotes/origin/main")
	if fetchedHead != remoteHead {
		t.Fatalf("fetched head = %q, want %q", fetchedHead, remoteHead)
	}
}

func TestBroker_GitBuiltinRestrictsWorktree(t *testing.T) {
	repo := initGitRepo(t)
	other := initGitRepo(t)
	remote := initBareRemote(t)

	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, gateGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     other,
		Token:   token,
		Git: &sandbox.GitRequest{
			Op:     sandbox.GitOpFetch,
			Remote: "origin",
		},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "restricted to the current worktree") {
		t.Fatalf("stderr = %q", resp.Stderr)
	}
}

func TestBroker_GitBuiltinRejectsUnknownRemote(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)

	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, gateGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Git: &sandbox.GitRequest{
			Op:     sandbox.GitOpPush,
			Remote: remote,
			Refspecs: []string{
				"main",
			},
		},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("stderr = %q", resp.Stderr)
	}
}

// hook role からの broker 経由 git push は拒否される。
// agent (Claude 等) が直接 origin に push してしまうと pr-verify gate と
// 競合して無限 rework ループを引き起こすため、role=hook では builtin git の
// push 操作を一律禁止する。
func TestBroker_GitBuiltinRejectsHookRolePush(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)

	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
		Role:       "hook",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Git: &sandbox.GitRequest{
			Op:     sandbox.GitOpPush,
			Remote: "origin",
		},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "not allowed for role hook") {
		t.Fatalf("stderr = %q, want 'not allowed for role hook'", resp.Stderr)
	}
}

// hook role からの broker 経由 git fetch も同様に拒否される。
// fetch も外部リモートと通信するため、外部通信を許可ドメインのみに制限する
// hook の方針と整合させる。
func TestBroker_GitBuiltinRejectsHookRoleFetch(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)

	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
		Role:       "hook",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Git: &sandbox.GitRequest{
			Op:     sandbox.GitOpFetch,
			Remote: "origin",
		},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "not allowed for role hook") {
		t.Fatalf("stderr = %q, want 'not allowed for role hook'", resp.Stderr)
	}
}

// gate role からの push は引き続き許可される (pr-verify gate が使う経路)。
func TestBroker_GitBuiltinAllowsGateRolePush(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)

	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	writeFile(t, filepath.Join(repo, "gate.txt"), "gate change\n")
	runGit(t, repo, "add", "gate.txt")
	runGit(t, repo, "commit", "-m", "gate")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, gateGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
		Role:       "gate",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Git: &sandbox.GitRequest{
			Op:     sandbox.GitOpPush,
			Remote: "origin",
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("git push exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}

	localHead := runGit(t, repo, "rev-parse", "HEAD")
	remoteHead := runGitBare(t, remote, "rev-parse", "refs/heads/main")
	if remoteHead != localHead {
		t.Fatalf("remote head = %q, want %q", remoteHead, localHead)
	}
}

const realGitForTest = "/usr/bin/git"

func skipWithoutRealGit(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(realGitForTest); err != nil {
		t.Skipf("%s not available", realGitForTest)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	skipWithoutRealGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Boid Test")
	runGit(t, dir, "config", "user.email", "boid@example.com")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func initBareRemote(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	remote := filepath.Join(parent, "remote.git")
	runGit(t, parent, "init", "--bare", remote)
	return remote
}

func cloneRepo(t *testing.T, remote string) string {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "clone")
	runGit(t, parent, "clone", "--branch", "main", remote, dir)
	runGit(t, dir, "config", "user.name", "Boid Peer")
	runGit(t, dir, "config", "user.email", "peer@example.com")
	return dir
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(realGitForTest, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func runGitBare(t *testing.T, gitDir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"--git-dir", gitDir}, args...)
	return runGit(t, "", fullArgs...)
}
