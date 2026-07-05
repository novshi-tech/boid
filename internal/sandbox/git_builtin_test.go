//go:build e2e

// このファイルは /usr/bin/git を直接 exec し、実リポジトリ相手に push/fetch
// する end-to-end 試験。ホスト環境 (本物の git / サンドボックス外 / 書き込み
// 可能な TempDir) を前提とするため、通常の go test ./... からは //go:build
// e2e タグで除外する。CI では go test -tags=e2e ./internal/sandbox/... で
// 走らせる。純粋ロジック単体試験は git_builtin_logic_test.go を参照。
package sandbox_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Local policy builders, keeping sandbox tests independent of orchestrator.
func fullGitPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"git": {AllowedOps: map[string]struct{}{
			string(sandbox.GitOpFetch): {},
			string(sandbox.GitOpPush):  {},
		}},
	}
}

func hookGitPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"git": {},
	}
}

func TestBroker_GitBuiltinPushUsesTrustedSnapshot(t *testing.T) {
	repo := initGitRepo(t)
	remote1 := initBareRemote(t)
	remote2 := initBareRemote(t)

	runGit(t, repo, "remote", "add", "origin", remote1)
	runGit(t, repo, "push", "-u", "origin", "main")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, fullGitPolicies(), sandbox.TokenContext{
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
		Args:    []string{"push", "origin"},
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

// 回帰テスト: トークン登録「後」に追加された remote (例: gh repo create が
// origin を足す) を、broker が push 時に拾えること。
//
// 再現元 (ubs-apps, 2026-06-06):
//   git init → トークン登録 (この時点で remote 無し → snapshot remotes=0)
//   → gh repo create で origin 追加 → git push が "remote must be specified"
//   で弾かれた。binding はトークン登録時に1回しか capture されないのが原因。
//
// 期待挙動: resolveGitRemote が snapshot で解決できなかったら worktree を
// 読み直して再解決する。既知 remote は snapshot から解決されるため
// TestBroker_GitBuiltinPushUsesTrustedSnapshot の URL 固定性は保たれる。
func TestBroker_GitBuiltinPush_RecapturesRemoteAddedAfterRegistration(t *testing.T) {
	repo := initGitRepo(t) // remote 無しの新規リポ (ubs-apps の git init 直後相当)
	remote := initBareRemote(t)

	broker := &sandbox.Broker{}
	// 登録時点では origin が無い → snapshot は remotes=0。
	token := broker.Register(nil, fullGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	// 登録後に origin を追加 (gh repo create 相当)。
	runGit(t, repo, "remote", "add", "origin", remote)

	// 明示 remote の push: snapshot に origin が無くても re-capture で解決される。
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"push", "origin", "main"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("explicit-remote push exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	remoteHead := runGitBare(t, remote, "rev-parse", "refs/heads/main")
	localHead := runGit(t, repo, "rev-parse", "HEAD")
	if remoteHead != localHead {
		t.Fatalf("remote head = %q, want %q", remoteHead, localHead)
	}
}

// 上と同じだが remote 名を省略した素の git push のケース。
// snapshot が空でも、re-capture 後に remote が1個だけなら自動採用される。
func TestBroker_GitBuiltinPush_BareRecapturesSingleRemote(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, fullGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	runGit(t, repo, "remote", "add", "origin", remote)

	// remote 省略 (req.Remote == "")。
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"push"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("bare push exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	remoteHead := runGitBare(t, remote, "rev-parse", "refs/heads/main")
	localHead := runGit(t, repo, "rev-parse", "HEAD")
	if remoteHead != localHead {
		t.Fatalf("remote head = %q, want %q", remoteHead, localHead)
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
	token := broker.Register(nil, fullGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"fetch", "origin"},
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
	token := broker.Register(nil, fullGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     other,
		Token:   token,
		Args:    []string{"fetch", "origin"},
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
	token := broker.Register(nil, fullGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"push", remote, "main"},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("stderr = %q", resp.Stderr)
	}
}

// hook role からの broker 経由 git push は拒否される。
// role=hook では builtin git の push 操作を一律禁止する。
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
		Args:    []string{"push", "origin"},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "not allowed by policy") {
		t.Fatalf("stderr = %q, want 'not allowed by policy'", resp.Stderr)
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
		Args:    []string{"fetch", "origin"},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "not allowed by policy") {
		t.Fatalf("stderr = %q, want 'not allowed by policy'", resp.Stderr)
	}
}

// --- broker 経由 direct exec テスト ---

// broker が direct subcommand (status 等) を args のみで受け取り、
// ワークツリーで実行して出力を返すことを確認する。
func TestBroker_GitDirectExec_StatusReturnsOutput(t *testing.T) {
	repo := initGitRepo(t)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"status", "--short"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("git status exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
}

// broker が direct subcommand を実行する際、req.Cwd がそのまま使われることを確認する。
// cwd がサブディレクトリの場合、WorktreeRoot ではなく実際の cwd で実行される必要がある。
// (git add . のような相対パス操作に影響するため)
func TestBroker_GitDirectExec_RespectsCwd(t *testing.T) {
	repo := initGitRepo(t)
	subdir := filepath.Join(repo, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	// git rev-parse --show-prefix はカレントディレクトリを基準にリポジトリルートからの
	// 相対パスを出力する。WorktreeRoot からだと "" だが subdir からだと "subdir/" になる。
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     subdir,
		Token:   token,
		Args:    []string{"rev-parse", "--show-prefix"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("git rev-parse exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	got := strings.TrimSpace(resp.Stdout)
	if got != "subdir/" {
		t.Errorf("show-prefix = %q, want %q (cwd not respected)", got, "subdir/")
	}
}

// broker 側で禁止 global option が全件 reject されることを確認する。
// タスク仕様で再確認要求されている全オプションをカバーする。
func TestBroker_GitDirectExec_DeniedGlobalOptions(t *testing.T) {
	repo := initGitRepo(t)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	cases := []struct {
		name string
		args []string
	}{
		{"dash-C", []string{"-C", "/tmp/other", "status"}},
		{"dash-c", []string{"-c", "core.hooksPath=/tmp", "status"}},
		{"git-dir", []string{"--git-dir=/tmp/other", "status"}},
		{"work-tree", []string{"--work-tree=/tmp/other", "status"}},
		{"namespace", []string{"--namespace=evil", "status"}},
		{"config-env", []string{"--config-env=X=Y", "status"}},
		{"double-dash-global", []string{"--", "status"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := broker.Handle(&sandbox.ExecRequest{
				Command: "git",
				Cwd:     repo,
				Token:   token,
				Args:    tc.args,
			})
			if resp.ExitCode == 0 {
				t.Fatalf("expected error for args %v", tc.args)
			}
			if !strings.Contains(resp.Stderr, "not allowed") {
				t.Fatalf("stderr = %q, want 'not allowed'", resp.Stderr)
			}
		})
	}
}

// broker 側で force/delete refspec が raw args 経由でも reject されることを確認する。
// 旧 shim では sandbox 側で検証していたが、broker 一本化後は broker 側で検証する必要がある。
func TestBroker_GitPush_RejectsForceAndDeleteRefspecs(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, fullGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	cases := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{
			"force-refspec",
			[]string{"push", "origin", "+main:main"},
			"force refspecs",
		},
		{
			"delete-refspec",
			[]string{"push", "origin", ":refs/heads/main"},
			"push_delete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := broker.Handle(&sandbox.ExecRequest{
				Command: "git",
				Cwd:     repo,
				Token:   token,
				Args:    tc.args,
			})
			if resp.ExitCode == 0 {
				t.Fatalf("expected error for args %v", tc.args)
			}
			if !strings.Contains(resp.Stderr, tc.wantMsg) {
				t.Fatalf("stderr = %q, want %q", resp.Stderr, tc.wantMsg)
			}
		})
	}
}

// allowlist に載っていない subcommand は broker 側で拒否される。
func TestBroker_GitDirectExec_DeniedSubcommand(t *testing.T) {
	repo := initGitRepo(t)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"pull", "origin", "main"},
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected error for 'git pull'")
	}
}

// broker が git config の dangerous write を拒否することを確認する。
func TestBroker_GitConfig_RejectsDangerousWrite(t *testing.T) {
	repo := initGitRepo(t)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	cases := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{
			"remote url",
			[]string{"config", "remote.origin.url", "https://evil.example.com"},
			"not allowed",
		},
		{
			"core.hooksPath",
			[]string{"config", "core.hooksPath", "/tmp/evil"},
			"not allowed",
		},
		{
			"core.sshCommand",
			[]string{"config", "core.sshCommand", "evil"},
			"not allowed",
		},
		{
			"filter.lfs.clean",
			[]string{"config", "filter.lfs.clean", "cat"},
			"not allowed",
		},
		{
			"credential.helper",
			[]string{"config", "credential.helper", "store"},
			"not allowed",
		},
		{
			"include.path",
			[]string{"config", "include.path", "/evil"},
			"not allowed",
		},
		{
			"--global scope",
			[]string{"config", "--global", "user.name", "evil"},
			"not allowed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := broker.Handle(&sandbox.ExecRequest{
				Command: "git",
				Cwd:     repo,
				Token:   token,
				Args:    tc.args,
			})
			if resp.ExitCode == 0 {
				t.Fatalf("expected error for args %v", tc.args)
			}
			if !strings.Contains(resp.Stderr, tc.wantMsg) {
				t.Fatalf("stderr = %q, want %q", resp.Stderr, tc.wantMsg)
			}
		})
	}
}

// broker が git config --get は許可することを確認する。
func TestBroker_GitConfig_AllowsGet(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)
	runGit(t, repo, "remote", "add", "origin", remote)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"config", "--get", "remote.origin.url"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("git config --get exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, remote) {
		t.Fatalf("stdout = %q, want %q", resp.Stdout, remote)
	}
}

// broker が user.name 等の非禁止 write を許可することを確認する。
func TestBroker_GitConfig_AllowsUserWrite(t *testing.T) {
	repo := initGitRepo(t)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"config", "user.name", "Boid Test"},
	})
	// 0 であること（実際に書き込めること）
	if resp.ExitCode != 0 {
		t.Fatalf("git config user.name exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
}

// git submodule add は broker 側で拒否される。
func TestBroker_GitSubmodule_IsRejected(t *testing.T) {
	repo := initGitRepo(t)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"submodule", "add", "https://github.com/evil/repo"},
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected error for 'git submodule add'")
	}
	if !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("stderr = %q, want 'not allowed'", resp.Stderr)
	}
}

// broker が git exec 時に hardening config を付与していることを確認する。
func TestBroker_GitBuiltin_HardeningArgs(t *testing.T) {
	repo := initGitRepo(t)
	remote := initBareRemote(t)
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	writeFile(t, filepath.Join(repo, "hardening.txt"), "hardening\n")
	runGit(t, repo, "add", "hardening.txt")
	runGit(t, repo, "commit", "-m", "hardening")

	broker := &sandbox.Broker{}
	token := broker.Register(nil, fullGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	// push が通常通り成功すること（hardening config で壊れていないことの確認）
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     repo,
		Token:   token,
		Args:    []string{"push", "origin"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("git push with hardening args failed: exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
}

// hook role からの direct git は sandbox cwd を維持する (subdirectory 起動の
// 挙動を保つ)。WorktreeRoot 配下の subdir からの呼び出しが --show-prefix で
// 期待通り解決されることを確認する。
func TestBroker_GitDirectExec_HookKeepsSandboxCwd(t *testing.T) {
	repo := initGitRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
		Role:       "hook",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     sub,
		Token:   token,
		Args:    []string{"rev-parse", "--show-prefix"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("git rev-parse failed: exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	if got := strings.TrimSpace(resp.Stdout); got != "sub/" {
		t.Fatalf("git --show-prefix = %q, want \"sub/\" (hook cwd should not be redirected)", got)
	}
}

// peer project 配下を cwd にした git plumbing (hash-object / commit) は
// "restricted to the current worktree" で拒否される。
// AllowedCwdRoots に HomeDir を含まない git policy 下では peer project の
// パスは WorktreeRoot 外になるため broker が弾く。
func TestBroker_GitDirectExec_RejectsPeerProjectForPlumbingCommands(t *testing.T) {
	repo := initGitRepo(t)
	peer := initGitRepo(t)

	broker := &sandbox.Broker{}
	// hookGitPolicies() は AllowedCwdRoots を持たないため
	// WorktreeRoot (= repo) 以外の cwd は全て弾かれる。
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	cases := []struct {
		name string
		args []string
	}{
		{"hash-object", []string{"hash-object", "--stdin"}},
		{"commit", []string{"commit", "--allow-empty", "-m", "evil"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := broker.Handle(&sandbox.ExecRequest{
				Command: "git",
				Cwd:     peer, // peer project cwd
				Token:   token,
				Args:    tc.args,
			})
			if resp.ExitCode == 0 {
				t.Fatalf("args=%v: expected rejection for peer project cwd, got exit=0", tc.args)
			}
			if !strings.Contains(resp.Stderr, "restricted to the current worktree") {
				t.Fatalf("args=%v: stderr = %q, want 'restricted to the current worktree'", tc.args, resp.Stderr)
			}
		})
	}
}

// direct exec でも cwd 制限は有効。
func TestBroker_GitDirectExec_RestrictsCwd(t *testing.T) {
	repo := initGitRepo(t)
	other := initGitRepo(t)

	broker := &sandbox.Broker{}
	token := broker.Register(nil, hookGitPolicies(), sandbox.TokenContext{
		ProjectID:  "proj-1",
		ProjectDir: repo,
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "git",
		Cwd:     other,
		Token:   token,
		Args:    []string{"status", "--short"},
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected error when cwd is outside worktree")
	}
	if !strings.Contains(resp.Stderr, "restricted to the current worktree") {
		t.Fatalf("stderr = %q, want 'restricted to the current worktree'", resp.Stderr)
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
