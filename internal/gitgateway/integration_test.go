package gitgateway

import (
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the gateway against a *real* git client and a real
// git-http-backend(1) CGI process serving a local bare repo
// (docs/plans/git-gateway-cutover.md PR3: 「httptest + cgi の単体テストで
// 転送細部... を検証」, and the parent e2e 戦略 節's fixture-upstream idea,
// applied here at unit-test scope instead of e2e scope). If `git` or
// git-http-backend can't be located (e.g. no git installed), tests skip
// rather than fail — this is an environment precondition, not a gateway bug.

// gitHTTPBackendPath locates the git-http-backend helper binary via
// `git --exec-path`, the portable way to find git's helper directory
// regardless of distro layout.
func gitHTTPBackendPath(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path unavailable, skipping git-http-backend integration test: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("git-http-backend not found at %q, skipping: %v", path, err)
	}
	return path
}

// runGit runs a git command in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitTestEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s) failed: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// runGitAllowError runs a git command in dir and returns its combined
// output and error without failing the test, for negative-path assertions.
func runGitAllowError(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitTestEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gitTestEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=gitgateway-test",
		"GIT_AUTHOR_EMAIL=gitgateway-test@example.com",
		"GIT_COMMITTER_NAME=gitgateway-test",
		"GIT_COMMITTER_EMAIL=gitgateway-test@example.com",
	)
}

// newBareUpstreamRepo creates a bare repo at <reposRoot>/<owner>/<repo>.git
// with http.receivepack enabled (git-http-backend refuses push over smart
// HTTP unless a repo opts in).
func newBareUpstreamRepo(t *testing.T, reposRoot, owner, repo string) string {
	t.Helper()
	bareDir := filepath.Join(reposRoot, owner, repo+".git")
	if err := os.MkdirAll(filepath.Dir(bareDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, reposRoot, "init", "--quiet", "--bare", bareDir)
	runGit(t, bareDir, "config", "http.receivepack", "true")
	return bareDir
}

// newGitHTTPBackendUpstream starts an httptest.Server backed by a real
// git-http-backend CGI process rooted at reposRoot.
func newGitHTTPBackendUpstream(t *testing.T, reposRoot string) *httptest.Server {
	t.Helper()
	h := &cgi.Handler{
		Path: gitHTTPBackendPath(t),
		Env: []string{
			"GIT_PROJECT_ROOT=" + reposRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH=" + os.Getenv("PATH"),
		},
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestIntegration_CloneCommitPushFetch(t *testing.T) {
	reposRoot := t.TempDir()
	newBareUpstreamRepo(t, reposRoot, "owner", "repo")
	upstream := newGitHTTPBackendUpstream(t, reposRoot)

	host := upstreamHost(t, upstream)
	repoKey := NewRepoKey(host, "owner", "repo")

	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{repoKey: PermFetchPush}, "default")
	creds := NewCredentialProvider([]HostForgeConfig{
		{Host: host, Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, func(string, string) (string, error) { return "unused-in-this-test", nil })
	gw := NewServer(reg, creds, nil)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	cloneURL := gwSrv.URL + "/j/" + token + "/" + host + "/owner/repo.git"

	workDir := t.TempDir()

	// 1. Clone the (empty) upstream repo through the gateway.
	cloneDir := filepath.Join(workDir, "clone1")
	runGit(t, workDir, "clone", "--quiet", cloneURL, cloneDir)

	// 2. Commit a file and push it back through the gateway.
	if err := os.WriteFile(filepath.Join(cloneDir, "hello.txt"), []byte("hello gateway\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, cloneDir, "add", "hello.txt")
	runGit(t, cloneDir, "commit", "--quiet", "-m", "add hello.txt")
	branch := strings.TrimSpace(runGit(t, cloneDir, "rev-parse", "--abbrev-ref", "HEAD"))
	runGit(t, cloneDir, "push", "--quiet", "origin", branch)

	// 3. Fresh clone (fetch path) proves the push actually landed upstream,
	// not just locally.
	cloneDir2 := filepath.Join(workDir, "clone2")
	runGit(t, workDir, "clone", "--quiet", cloneURL, cloneDir2)
	content, err := os.ReadFile(filepath.Join(cloneDir2, "hello.txt"))
	if err != nil {
		t.Fatalf("reading pushed file from second clone: %v", err)
	}
	if string(content) != "hello gateway\n" {
		t.Fatalf("content = %q, want %q", content, "hello gateway\n")
	}

	// 4. The ".git"-suffix-free URL form must resolve to the same repo
	// (docs/plans/git-gateway-cutover.md PR3 設計調整: 両 suffix form の吸収).
	cloneURLNoSuffix := gwSrv.URL + "/j/" + token + "/" + host + "/owner/repo"
	cloneDir3 := filepath.Join(workDir, "clone3")
	runGit(t, workDir, "clone", "--quiet", cloneURLNoSuffix, cloneDir3)
	content3, err := os.ReadFile(filepath.Join(cloneDir3, "hello.txt"))
	if err != nil {
		t.Fatalf("reading file via suffix-free clone URL: %v", err)
	}
	if string(content3) != "hello gateway\n" {
		t.Fatalf("suffix-free clone content = %q, want %q", content3, "hello gateway\n")
	}
}

func TestIntegration_FetchOnlyTokenRejectsPush(t *testing.T) {
	reposRoot := t.TempDir()
	newBareUpstreamRepo(t, reposRoot, "owner", "repo")
	upstream := newGitHTTPBackendUpstream(t, reposRoot)

	host := upstreamHost(t, upstream)
	repoKey := NewRepoKey(host, "owner", "repo")

	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{repoKey: PermFetch}, "default") // fetch-only, e.g. a readonly job or workspace peer
	creds := NewCredentialProvider([]HostForgeConfig{
		{Host: host, Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, func(string, string) (string, error) { return "unused", nil })
	gw := NewServer(reg, creds, nil)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	cloneURL := gwSrv.URL + "/j/" + token + "/" + host + "/owner/repo.git"
	workDir := t.TempDir()
	cloneDir := filepath.Join(workDir, "clone")
	runGit(t, workDir, "clone", "--quiet", cloneURL, cloneDir)

	if err := os.WriteFile(filepath.Join(cloneDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, cloneDir, "add", "x.txt")
	runGit(t, cloneDir, "commit", "--quiet", "-m", "x")
	branch := strings.TrimSpace(runGit(t, cloneDir, "rev-parse", "--abbrev-ref", "HEAD"))

	out, err := runGitAllowError(t, cloneDir, "push", "origin", branch)
	if err == nil {
		t.Fatalf("expected push to fail for a fetch-only token; output:\n%s", out)
	}
	if !strings.Contains(out, "403") {
		t.Fatalf("expected the gateway's 403 to surface in push output, got:\n%s", out)
	}
}

func TestIntegration_ForbiddenRepoRejectsClone(t *testing.T) {
	reposRoot := t.TempDir()
	newBareUpstreamRepo(t, reposRoot, "owner", "repo")
	upstream := newGitHTTPBackendUpstream(t, reposRoot)

	host := upstreamHost(t, upstream)
	// Register the token for a *different* repo than the one we'll try to
	// clone, mirroring "許可外の repo... は 403" (workspace peer / read-only
	// extra-repo allowlists that don't include this repo).
	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{NewRepoKey(host, "owner", "other"): PermFetch}, "default")
	creds := NewCredentialProvider([]HostForgeConfig{
		{Host: host, Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, func(string, string) (string, error) { return "unused", nil })
	gw := NewServer(reg, creds, nil)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	cloneURL := gwSrv.URL + "/j/" + token + "/" + host + "/owner/repo.git"
	workDir := t.TempDir()
	cloneDir := filepath.Join(workDir, "clone")

	out, err := runGitAllowError(t, workDir, "clone", "--quiet", cloneURL, cloneDir)
	if err == nil {
		t.Fatalf("expected clone to fail for a repo outside the token's allowed set; output:\n%s", out)
	}
	if !strings.Contains(out, "403") {
		t.Fatalf("expected the gateway's 403 to surface in clone output, got:\n%s", out)
	}
}

func TestIntegration_InvalidTokenRejectsClone(t *testing.T) {
	reposRoot := t.TempDir()
	newBareUpstreamRepo(t, reposRoot, "owner", "repo")
	upstream := newGitHTTPBackendUpstream(t, reposRoot)
	host := upstreamHost(t, upstream)

	creds := NewCredentialProvider([]HostForgeConfig{
		{Host: host, Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, func(string, string) (string, error) { return "unused", nil })
	gw := NewServer(NewRegistry(), creds, nil)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	cloneURL := gwSrv.URL + "/j/not-a-registered-token/" + host + "/owner/repo.git"
	workDir := t.TempDir()

	out, err := runGitAllowError(t, workDir, "clone", "--quiet", cloneURL, filepath.Join(workDir, "clone"))
	if err == nil {
		t.Fatalf("expected clone to fail for an invalid token; output:\n%s", out)
	}
	// A bare 401 (no WWW-Authenticate challenge) makes git's own HTTP client
	// try to prompt for credentials rather than surfacing "401" in its error
	// text; with prompts disabled (GIT_TERMINAL_PROMPT=0, required for any
	// non-interactive/automated clone — the runner's eventual clone in PR5
	// will need the same) it fails with "could not read Username" instead.
	// Either string is proof the request was rejected before reaching
	// upstream, which is what this test is actually checking.
	if !strings.Contains(out, "401") && !strings.Contains(out, "could not read Username") {
		t.Fatalf("expected the gateway's 401 to surface in clone output, got:\n%s", out)
	}
}
