package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// これらは実 git バイナリを一切呼ばないロジック単体テスト。
// end-to-end (broker.Register → exec.Command(realGit)) は git_builtin_test.go
// (//go:build e2e) 側でカバーする。

// ---------- validateGitBuiltinCwd ----------

func TestValidateGitBuiltinCwd(t *testing.T) {
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	allowedExtra := filepath.Join(tmp, "extra")
	if err := os.MkdirAll(allowedExtra, 0o755); err != nil {
		t.Fatalf("mkdir extra: %v", err)
	}
	regularFile := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(regularFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	entry := &tokenEntry{
		BuiltinPolicies: map[string]BuiltinPolicy{
			"git": {AllowedCwdRoots: []string{allowedExtra}},
		},
		Git: &GitBinding{WorktreeRoot: worktree},
	}

	cases := []struct {
		name    string
		cwd     string
		wantErr string // "" means expect nil
	}{
		{"empty", "", "cwd required"},
		{"relative", "relative/path", "cwd must be absolute"},
		{"nonexistent", filepath.Join(tmp, "nope"), "cwd does not exist"},
		{"file not dir", regularFile, "cwd must be a directory"},
		{"outside worktree", tmp, "restricted to the current worktree"},
		{"inside worktree", worktree, ""},
		{"subdir of worktree", filepath.Join(worktree, "sub"), ""},
		{"inside allowed extra root", allowedExtra, ""},
	}

	if err := os.MkdirAll(filepath.Join(worktree, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitBuiltinCwd(tc.cwd, entry)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// validateGitBuiltinCwd は HomeDir 配下の peer project パスを拒否する。
// gitPolicy は HomeDir を AllowedCwdRoots に含まないため、peer project の
// cwd は WorktreeRoot でも AllowedCwdRoots でもなく弾かれる。
func TestValidateGitBuiltinCwd_PeerProjectRejectedWhenNotInPolicy(t *testing.T) {
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	projectDir := filepath.Join(tmp, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	// peer project はホスト HOME 配下にあるが AllowedCwdRoots に homeDir を含まない。
	homeDir := filepath.Join(tmp, "home")
	peerProject := filepath.Join(homeDir, "src", "peer-project")
	if err := os.MkdirAll(peerProject, 0o755); err != nil {
		t.Fatalf("mkdir peer: %v", err)
	}

	// gitPolicy の新実装: homeDir を含まない (projectDir と /tmp のみ)。
	entry := &tokenEntry{
		BuiltinPolicies: map[string]BuiltinPolicy{
			"git": {AllowedCwdRoots: []string{projectDir}},
		},
		Git: &GitBinding{WorktreeRoot: worktree},
	}

	err := validateGitBuiltinCwd(peerProject, entry)
	if err == nil {
		t.Fatal("peer project under homeDir should be rejected when homeDir is not in AllowedCwdRoots")
	}
	if !strings.Contains(err.Error(), "restricted to the current worktree") {
		t.Fatalf("error = %q, want 'restricted to the current worktree'", err.Error())
	}
}

// ---------- resolveGitRemote ----------

func TestResolveGitRemote(t *testing.T) {
	t.Run("explicit known", func(t *testing.T) {
		binding := &GitBinding{Remotes: map[string]GitRemote{
			"origin": {FetchURL: "fetch-url", PushURL: "push-url"},
		}}
		name, remote, err := resolveGitRemote(&GitRequest{Remote: "origin"}, binding)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "origin" || remote.FetchURL != "fetch-url" {
			t.Fatalf("got (%q,%+v), want (origin, fetch-url)", name, remote)
		}
	})

	t.Run("explicit unknown", func(t *testing.T) {
		binding := &GitBinding{Remotes: map[string]GitRemote{
			"origin": {FetchURL: "fetch-url"},
		}}
		_, _, err := resolveGitRemote(&GitRequest{Remote: "other"}, binding)
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("got err=%v, want 'not allowed'", err)
		}
	})

	t.Run("implicit single remote", func(t *testing.T) {
		binding := &GitBinding{Remotes: map[string]GitRemote{
			"only": {FetchURL: "u"},
		}}
		name, _, err := resolveGitRemote(&GitRequest{}, binding)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "only" {
			t.Fatalf("got %q, want only", name)
		}
	})

	t.Run("upstream fallback when multiple remotes", func(t *testing.T) {
		binding := &GitBinding{
			Remotes: map[string]GitRemote{
				"origin":   {FetchURL: "o"},
				"upstream": {FetchURL: "u"},
			},
			Upstream: GitUpstream{Remote: "upstream", MergeRef: "refs/heads/main"},
		}
		name, _, err := resolveGitRemote(&GitRequest{}, binding)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "upstream" {
			t.Fatalf("got %q, want upstream", name)
		}
	})

	t.Run("ambiguous multi remote no upstream", func(t *testing.T) {
		binding := &GitBinding{Remotes: map[string]GitRemote{
			"a": {FetchURL: "a"},
			"b": {FetchURL: "b"},
		}}
		_, _, err := resolveGitRemote(&GitRequest{}, binding)
		if err == nil || !strings.Contains(err.Error(), "must be specified") {
			t.Fatalf("got err=%v, want 'must be specified'", err)
		}
	})
}

// ---------- resolveGitFetchRefspecs ----------

func TestResolveGitFetchRefspecs(t *testing.T) {
	t.Run("bare branch names expanded to tracking refspecs", func(t *testing.T) {
		binding := &GitBinding{}
		got, err := resolveGitFetchRefspecs(&GitRequest{Refspecs: []string{"main", "dev"}}, binding, "origin")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{
			"refs/heads/main:refs/remotes/origin/main",
			"refs/heads/dev:refs/remotes/origin/dev",
		}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("explicit dst refspec passed through unchanged", func(t *testing.T) {
		binding := &GitBinding{}
		got, err := resolveGitFetchRefspecs(&GitRequest{Refspecs: []string{"main:refs/heads/local-main"}}, binding, "origin")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != "main:refs/heads/local-main" {
			t.Fatalf("got %v, want [main:refs/heads/local-main]", got)
		}
	})

	t.Run("upstream synthesis", func(t *testing.T) {
		binding := &GitBinding{
			Upstream: GitUpstream{Remote: "origin", MergeRef: "refs/heads/main"},
		}
		got, err := resolveGitFetchRefspecs(&GitRequest{}, binding, "origin")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "refs/heads/main:refs/remotes/origin/main"
		if len(got) != 1 || got[0] != want {
			t.Fatalf("got %v, want [%s]", got, want)
		}
	})

	t.Run("upstream mismatch fails", func(t *testing.T) {
		binding := &GitBinding{
			Upstream: GitUpstream{Remote: "origin", MergeRef: "refs/heads/main"},
		}
		_, err := resolveGitFetchRefspecs(&GitRequest{}, binding, "other-remote")
		if err == nil || !strings.Contains(err.Error(), "upstream") {
			t.Fatalf("got err=%v, want 'upstream'", err)
		}
	})

	t.Run("no refspec no upstream", func(t *testing.T) {
		binding := &GitBinding{}
		_, err := resolveGitFetchRefspecs(&GitRequest{}, binding, "origin")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
	})
}

// ---------- expandFetchRefspec ----------

func TestExpandFetchRefspec(t *testing.T) {
	cases := []struct {
		refspec  string
		remote   string
		want     string
	}{
		// bare branch name → expanded
		{"main", "origin", "refs/heads/main:refs/remotes/origin/main"},
		{"dev", "upstream", "refs/heads/dev:refs/remotes/upstream/dev"},
		// refs/heads/ prefix → expanded
		{"refs/heads/main", "origin", "refs/heads/main:refs/remotes/origin/main"},
		// force prefix preserved
		{"+main", "origin", "+refs/heads/main:refs/remotes/origin/main"},
		{"+refs/heads/main", "origin", "+refs/heads/main:refs/remotes/origin/main"},
		// explicit dst → unchanged
		{"main:refs/heads/local", "origin", "main:refs/heads/local"},
		// special refs → unchanged
		{"HEAD", "origin", "HEAD"},
		{"refs/tags/v1.0", "origin", "refs/tags/v1.0"},
		{"refs/notes/commits", "origin", "refs/notes/commits"},
		// other full refs → unchanged
		{"refs/pull/42/head", "origin", "refs/pull/42/head"},
	}
	for _, tc := range cases {
		got := expandFetchRefspec(tc.refspec, tc.remote)
		if got != tc.want {
			t.Errorf("expandFetchRefspec(%q, %q) = %q, want %q", tc.refspec, tc.remote, got, tc.want)
		}
	}
}

// ---------- resolveGitPushRefspecs ----------

func TestResolveGitPushRefspecs_Explicit(t *testing.T) {
	// refspecs が明示されている場合だけ pure（未指定は実 git の
	// symbolic-ref 呼び出しが走るので e2e 側でカバー）。
	binding := &GitBinding{}
	got, err := resolveGitPushRefspecs(&GitRequest{Refspecs: []string{"HEAD:refs/heads/main"}}, binding, "origin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "HEAD:refs/heads/main" {
		t.Fatalf("got %v, want [HEAD:refs/heads/main]", got)
	}
}

// ---------- buildGitBuiltinArgs ----------

func hardeningPrefix() []string {
	return []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.attributesfile=/dev/null",
		"-c", "core.editor=false",
	}
}

func TestBuildGitBuiltinArgs_Fetch(t *testing.T) {
	binding := &GitBinding{
		Remotes: map[string]GitRemote{
			"origin": {FetchURL: "https://example.com/repo"},
		},
	}

	t.Run("bare branch name expanded to tracking refspec", func(t *testing.T) {
		req := &GitRequest{Op: GitOpFetch, Remote: "origin", Refspecs: []string{"main"}}
		args, err := buildGitBuiltinArgs(req, binding, "origin", binding.Remotes["origin"])
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := append(hardeningPrefix(), "fetch", "https://example.com/repo", "refs/heads/main:refs/remotes/origin/main")
		assertArgs(t, args, want)
	})

	t.Run("all flags preserved", func(t *testing.T) {
		req := &GitRequest{
			Op: GitOpFetch, Remote: "origin", Refspecs: []string{"main"},
			DryRun: true, Verbose: true, Quiet: true, Prune: true, Tags: true, Force: true,
		}
		args, err := buildGitBuiltinArgs(req, binding, "origin", binding.Remotes["origin"])
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := append(hardeningPrefix(), "fetch",
			"--dry-run", "--verbose", "--quiet", "--prune", "--tags", "--force",
			"https://example.com/repo", "refs/heads/main:refs/remotes/origin/main",
		)
		assertArgs(t, args, want)
	})

	t.Run("missing fetch url fails", func(t *testing.T) {
		remote := GitRemote{} // FetchURL 空
		_, err := buildGitBuiltinArgs(&GitRequest{Op: GitOpFetch, Remote: "origin", Refspecs: []string{"main"}}, binding, "origin", remote)
		if err == nil || !strings.Contains(err.Error(), "no fetch url") {
			t.Fatalf("got err=%v, want 'no fetch url'", err)
		}
	})
}

func TestBuildGitBuiltinArgs_Push(t *testing.T) {
	binding := &GitBinding{
		Remotes: map[string]GitRemote{
			"origin": {FetchURL: "https://fetch.example/repo", PushURL: "https://push.example/repo"},
		},
	}

	t.Run("explicit refspec uses push url", func(t *testing.T) {
		req := &GitRequest{Op: GitOpPush, Remote: "origin", Refspecs: []string{"HEAD:refs/heads/main"}}
		args, err := buildGitBuiltinArgs(req, binding, "origin", binding.Remotes["origin"])
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := append(hardeningPrefix(), "push", "https://push.example/repo", "HEAD:refs/heads/main")
		assertArgs(t, args, want)
	})

	t.Run("fallback to fetch url when push url empty", func(t *testing.T) {
		remote := GitRemote{FetchURL: "https://fetch.example/repo"}
		req := &GitRequest{Op: GitOpPush, Remote: "origin", Refspecs: []string{"HEAD:refs/heads/main"}}
		args, err := buildGitBuiltinArgs(req, binding, "origin", remote)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := append(hardeningPrefix(), "push", "https://fetch.example/repo", "HEAD:refs/heads/main")
		assertArgs(t, args, want)
	})

	t.Run("both urls empty fails", func(t *testing.T) {
		remote := GitRemote{}
		_, err := buildGitBuiltinArgs(&GitRequest{Op: GitOpPush, Remote: "origin", Refspecs: []string{"x"}}, binding, "origin", remote)
		if err == nil || !strings.Contains(err.Error(), "no push url") {
			t.Fatalf("got err=%v, want 'no push url'", err)
		}
	})

	t.Run("all flags preserved", func(t *testing.T) {
		req := &GitRequest{
			Op: GitOpPush, Remote: "origin", Refspecs: []string{"HEAD:main"},
			DryRun: true, Verbose: true, Quiet: true, Porcelain: true, ForceWithLease: true,
		}
		args, err := buildGitBuiltinArgs(req, binding, "origin", binding.Remotes["origin"])
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := append(hardeningPrefix(), "push",
			"--dry-run", "--verbose", "--quiet", "--porcelain", "--force-with-lease",
			"https://push.example/repo", "HEAD:main",
		)
		assertArgs(t, args, want)
	})
}

func TestBuildGitBuiltinArgs_UnsupportedOp(t *testing.T) {
	binding := &GitBinding{Remotes: map[string]GitRemote{"origin": {FetchURL: "u"}}}
	_, err := buildGitBuiltinArgs(&GitRequest{Op: GitOp("merge")}, binding, "origin", binding.Remotes["origin"])
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("got err=%v, want 'unsupported'", err)
	}
}

// ---------- handleGitBuiltinRequest (dispatch-level guards) ----------

func TestHandleGitBuiltinRequest_NoPolicy(t *testing.T) {
	entry := &tokenEntry{}
	resp := handleGitBuiltinRequest(&ExecRequest{Command: "git", Cwd: "/tmp"}, entry)
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed") {
		t.Fatalf("got exit=%d stderr=%q, want ExitCode=1 and 'not allowed'", resp.ExitCode, resp.Stderr)
	}
}

func TestHandleGitBuiltinRequest_NoBinding(t *testing.T) {
	entry := &tokenEntry{
		BuiltinPolicies: map[string]BuiltinPolicy{"git": {AllowedOps: map[string]struct{}{"push": {}}}},
		Git:             nil,
	}
	resp := handleGitBuiltinRequest(&ExecRequest{Command: "git", Cwd: "/tmp"}, entry)
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("got exit=%d stderr=%q, want ExitCode=1 and 'unavailable'", resp.ExitCode, resp.Stderr)
	}
}

func TestHandleGitBuiltinRequest_OpNotAllowed(t *testing.T) {
	tmp := t.TempDir()
	entry := &tokenEntry{
		BuiltinPolicies: map[string]BuiltinPolicy{"git": {AllowedOps: map[string]struct{}{"fetch": {}}}},
		Git:             &GitBinding{WorktreeRoot: tmp},
	}
	// Push は AllowedOps に無い → reject される。
	resp := handleGitBuiltinRequest(&ExecRequest{
		Command: "git",
		Cwd:     tmp,
		Args:    []string{"push", "origin"},
	}, entry)
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed by policy") {
		t.Fatalf("got exit=%d stderr=%q, want ExitCode=1 and 'not allowed by policy'", resp.ExitCode, resp.Stderr)
	}
}

// ---------- validateGitCloneLocal ----------

func TestValidateGitCloneLocal_SourceMustBePeer(t *testing.T) {
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "worktree")
	peerDir := filepath.Join(tmp, "peer")
	for _, d := range []string{worktree, peerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	entry := &tokenEntry{
		Context: TokenContext{
			WorkspacePeers: map[string]string{"peer-id": peerDir},
		},
		BuiltinPolicies: map[string]BuiltinPolicy{
			"git": {AllowedOps: map[string]struct{}{string(GitOpCloneLocal): {}}},
		},
		Git: &GitBinding{WorktreeRoot: worktree},
	}

	// source not in peers → rejected
	dest := filepath.Join(worktree, "clone")
	resp := handleGitBuiltinRequest(&ExecRequest{Command: "git", Cwd: worktree, Args: []string{"clone", "--local", "/tmp/notapeer", dest}}, entry)
	if resp.ExitCode == 0 {
		t.Fatal("expected error: source not in workspace peers")
	}
	if !strings.Contains(resp.Stderr, "source") {
		t.Fatalf("stderr = %q, want 'source'", resp.Stderr)
	}
}

func TestValidateGitCloneLocal_DestMustBeInWorktree(t *testing.T) {
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "worktree")
	peerDir := filepath.Join(tmp, "peer")
	for _, d := range []string{worktree, peerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	entry := &tokenEntry{
		Context: TokenContext{
			WorkspacePeers: map[string]string{"peer-id": peerDir},
		},
		BuiltinPolicies: map[string]BuiltinPolicy{
			"git": {AllowedOps: map[string]struct{}{string(GitOpCloneLocal): {}}},
		},
		Git: &GitBinding{WorktreeRoot: worktree},
	}

	// dest outside worktree → rejected
	resp := handleGitBuiltinRequest(&ExecRequest{Command: "git", Cwd: worktree, Args: []string{"clone", "--local", peerDir, "/tmp/outside"}}, entry)
	if resp.ExitCode == 0 {
		t.Fatal("expected error: dest outside worktree")
	}
	if !strings.Contains(resp.Stderr, "dest") {
		t.Fatalf("stderr = %q, want 'dest'", resp.Stderr)
	}
}

func TestValidateGitCloneLocal_PathTraversal(t *testing.T) {
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "worktree")
	peerDir := filepath.Join(tmp, "peer")
	for _, d := range []string{worktree, peerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	entry := &tokenEntry{
		Context: TokenContext{
			WorkspacePeers: map[string]string{"peer-id": peerDir},
		},
		BuiltinPolicies: map[string]BuiltinPolicy{
			"git": {AllowedOps: map[string]struct{}{string(GitOpCloneLocal): {}}},
		},
		Git: &GitBinding{WorktreeRoot: worktree},
	}

	// source with path traversal attempting to escape peer
	traversalSrc := peerDir + "/../../../etc"
	dest := filepath.Join(worktree, "clone")
	resp := handleGitBuiltinRequest(&ExecRequest{Command: "git", Cwd: worktree, Args: []string{"clone", "--local", traversalSrc, dest}}, entry)
	if resp.ExitCode == 0 {
		t.Fatal("expected error: path traversal in source")
	}

	// dest with path traversal attempting to escape worktree
	traversalDest := worktree + "/../../../tmp/evil"
	resp2 := handleGitBuiltinRequest(&ExecRequest{Command: "git", Cwd: worktree, Args: []string{"clone", "--local", peerDir, traversalDest}}, entry)
	if resp2.ExitCode == 0 {
		t.Fatal("expected error: path traversal in dest")
	}
}

func TestValidateGitCloneLocal_EmptyPeersRejects(t *testing.T) {
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "worktree")
	peerDir := filepath.Join(tmp, "peer")
	for _, d := range []string{worktree, peerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// no workspace peers configured
	entry := &tokenEntry{
		Context: TokenContext{WorkspacePeers: nil},
		BuiltinPolicies: map[string]BuiltinPolicy{
			"git": {AllowedOps: map[string]struct{}{string(GitOpCloneLocal): {}}},
		},
		Git: &GitBinding{WorktreeRoot: worktree},
	}

	dest := filepath.Join(worktree, "clone")
	resp := handleGitBuiltinRequest(&ExecRequest{Command: "git", Cwd: worktree, Args: []string{"clone", "--local", peerDir, dest}}, entry)
	if resp.ExitCode == 0 {
		t.Fatal("expected error: no workspace peers")
	}
}

// ---------- setGitUpstreamConfig ----------

func initGitRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	type step struct {
		args []string
		dir  string
	}
	steps := []step{
		{args: []string{"init", "-b", branch, dir}},
		{args: []string{"config", "user.email", "test@test.com"}, dir: dir},
		{args: []string{"config", "user.name", "Test"}, dir: dir},
		{args: []string{"commit", "--allow-empty", "-m", "init"}, dir: dir},
	}
	for _, s := range steps {
		cmd := exec.Command("git", s.args...)
		cmd.Dir = s.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", s.args, err, out)
		}
	}
	return dir
}

func TestSetGitUpstreamConfig_DefaultRefspec(t *testing.T) {
	dir := initGitRepo(t, "feature")
	binding := &GitBinding{
		WorktreeRoot: dir,
		Remotes: map[string]GitRemote{
			"origin": {FetchURL: "https://example.com/repo"},
		},
	}
	req := &GitRequest{Op: GitOpPush, Remote: "origin"}

	if err := setGitUpstreamConfig(dir, "origin", req, binding); err != nil {
		t.Fatalf("setGitUpstreamConfig: %v", err)
	}

	remote, err := gitOutput(dir, "config", "branch.feature.remote")
	if err != nil {
		t.Fatalf("read branch.feature.remote: %v", err)
	}
	if remote != "origin" {
		t.Fatalf("branch.feature.remote = %q, want %q", remote, "origin")
	}

	merge, err := gitOutput(dir, "config", "branch.feature.merge")
	if err != nil {
		t.Fatalf("read branch.feature.merge: %v", err)
	}
	if merge != "refs/heads/feature" {
		t.Fatalf("branch.feature.merge = %q, want %q", merge, "refs/heads/feature")
	}
}

func TestSetGitUpstreamConfig_ExplicitRefspec(t *testing.T) {
	dir := initGitRepo(t, "local-branch")
	binding := &GitBinding{
		WorktreeRoot: dir,
		Remotes: map[string]GitRemote{
			"origin": {FetchURL: "https://example.com/repo"},
		},
	}
	req := &GitRequest{Op: GitOpPush, Remote: "origin", Refspecs: []string{"HEAD:refs/heads/remote-branch"}}

	if err := setGitUpstreamConfig(dir, "origin", req, binding); err != nil {
		t.Fatalf("setGitUpstreamConfig: %v", err)
	}

	merge, err := gitOutput(dir, "config", "branch.local-branch.merge")
	if err != nil {
		t.Fatalf("read branch.local-branch.merge: %v", err)
	}
	if merge != "refs/heads/remote-branch" {
		t.Fatalf("branch.local-branch.merge = %q, want %q", merge, "refs/heads/remote-branch")
	}
}

// ---------- helpers ----------

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args length = %d, want %d\n  got:  %v\n  want: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q\n  got:  %v\n  want: %v", i, got[i], want[i], got, want)
		}
	}
}
