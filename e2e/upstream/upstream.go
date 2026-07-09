// Package upstream provides a fixture git-over-HTTP server for boid's e2e
// tests. It serves real bare git repositories via `git http-backend` (Go's
// standard net/http/cgi + a real git binary), so e2e scenario projects can
// have a real, reachable origin remote instead of the placeholder response
// hardcoded in e2e/fixtures/hostbin/git's `config` handler.
//
// See docs/plans/git-gateway-cutover.md, "e2e 戦略 (cutover より前に必要)"
// and "PR7: e2e" (PR7a): without this, cutover (PR6) would leave every e2e
// scenario's project without a reachable origin, and PR2's "origin の無い
// project は登録拒否" would make every scenario fail to dispatch.
package upstream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// defaultGitBin is the conventional real-git path already relied on
// elsewhere in this repo's e2e harness (e.g.
// e2e/scenarios/git-peer-clone-local/scenario.sh) to bypass the fake host
// git shim installed at the front of $PATH during e2e runs
// (e2e/fixtures/hostbin/git). CLAUDE.md restricts this project to Linux,
// and CI runs Ubuntu, where this path is stable.
const defaultGitBin = "/usr/bin/git"

// Options configures a fixture upstream git server.
type Options struct {
	// Dir is the parent directory bare repositories are created under. If
	// empty, New creates a temp directory and removes it on Close.
	Dir string
	// Addr is the TCP listen address. Defaults to "127.0.0.1:0" (OS-assigned
	// port) when empty.
	Addr string
	// GitBin is the real git binary to invoke. Defaults to /usr/bin/git
	// when that exists, else whatever `git` resolves to on PATH. Getting
	// this right matters inside the e2e harness, where plain PATH lookup
	// would resolve to the fake host git shim instead of a real git.
	GitBin string
}

// Upstream is a fixture git-over-HTTP server backed by real bare
// repositories.
type Upstream struct {
	dir    string
	ownDir bool
	ln     net.Listener
	srv    *http.Server
	gitBin string

	closeOnce sync.Once
	closeErr  error
}

// New starts a fixture upstream server listening on opts.Addr (or an
// OS-assigned loopback port). The caller must call Close when done; tests
// should use Serve instead, which wires that up automatically.
func New(opts Options) (*Upstream, error) {
	dir := opts.Dir
	ownDir := false
	if dir == "" {
		d, err := os.MkdirTemp("", "boid-e2e-upstream-")
		if err != nil {
			return nil, fmt.Errorf("upstream: create temp dir: %w", err)
		}
		dir = d
		ownDir = true
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("upstream: create dir %s: %w", dir, err)
	}
	// GIT_PROJECT_ROOT must be absolute: git-http-backend resolves it as-is,
	// with no cwd fallback of its own.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}

	gitBin := opts.GitBin
	if gitBin == "" {
		gitBin = resolveGitBin()
	}

	backend, execPath, err := findGitHTTPBackend(gitBin)
	if err != nil {
		if ownDir {
			_ = os.RemoveAll(dir)
		}
		return nil, err
	}

	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if ownDir {
			_ = os.RemoveAll(dir)
		}
		return nil, fmt.Errorf("upstream: listen %s: %w", addr, err)
	}

	handler := &cgi.Handler{
		Path: backend,
		Dir:  dir,
		Env: []string{
			"GIT_PROJECT_ROOT=" + dir,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_EXEC_PATH=" + execPath,
			// Deliberately not inherited from the parent process's PATH
			// (via cgi.Handler.InheritEnv): inside the e2e harness that
			// PATH is shadowed by the fake host git shim
			// (e2e/fixtures/hostbin/git), which git-http-backend must not
			// pick up when it shells out to git-upload-pack /
			// git-receive-pack.
			"PATH=" + execPath + ":/usr/bin:/bin",
		},
	}

	srv := &http.Server{Handler: handler}
	u := &Upstream{dir: dir, ownDir: ownDir, ln: ln, srv: srv, gitBin: gitBin}

	go func() {
		_ = srv.Serve(ln)
	}()

	return u, nil
}

// Serve is the test-oriented constructor: it fails t via Fatalf on error and
// registers a t.Cleanup that closes the server (and removes its directory,
// when owned).
func Serve(t *testing.T, opts Options) *Upstream {
	t.Helper()
	u, err := New(opts)
	if err != nil {
		t.Fatalf("upstream.Serve: %v", err)
	}
	t.Cleanup(func() {
		if err := u.Close(); err != nil {
			t.Logf("upstream.Close: %v", err)
		}
	})
	return u
}

// Addr returns the "host:port" the server is listening on.
func (u *Upstream) Addr() string {
	return u.ln.Addr().String()
}

// BaseURL returns "http://<addr>".
func (u *Upstream) BaseURL() string {
	return "http://" + u.Addr()
}

// Dir returns the parent directory bare repositories are stored under.
func (u *Upstream) Dir() string {
	return u.dir
}

// NewRepo creates (idempotently) a bare repository named name (".git" is
// appended if not already present) with `http.receivepack` enabled so
// clients can push to it over smart HTTP.
func (u *Upstream) NewRepo(name string) (string, error) {
	return InitBareRepo(u.gitBin, u.dir, name)
}

// URL returns the clone URL for the named repo (".git" appended if not
// already present). It does not verify the repo exists; call NewRepo first.
func (u *Upstream) URL(name string) string {
	return u.BaseURL() + "/" + repoDirName(name)
}

// Close shuts the HTTP server down and, when the Upstream owns its
// directory (Options.Dir was empty), removes it. Safe to call more than
// once.
func (u *Upstream) Close() error {
	u.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		u.closeErr = u.srv.Shutdown(ctx)
		if u.ownDir {
			_ = os.RemoveAll(u.dir)
		}
	})
	return u.closeErr
}

// InitBareRepo creates (idempotently) a bare git repository at
// <dir>/<name>.git using gitBin (empty resolves to the same default New
// uses), with `http.receivepack` enabled. It is exported as a standalone
// function (not tied to a running Upstream) so both Upstream.NewRepo and
// the e2e harness's one-shot `boid-e2e upstream-serve` startup share one
// implementation.
func InitBareRepo(gitBin, dir, name string) (string, error) {
	if gitBin == "" {
		gitBin = resolveGitBin()
	}
	repoPath := filepath.Join(dir, repoDirName(name))
	if _, err := os.Stat(repoPath); err == nil {
		return repoPath, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("upstream: stat %s: %w", repoPath, err)
	}

	initCmd := exec.Command(gitBin, "init", "--quiet", "--bare", "-b", "main", repoPath)
	if out, err := initCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("upstream: git init --bare %s: %w: %s", repoPath, err, out)
	}

	// Anonymous smart-HTTP receive-pack is disabled by default for safety;
	// this fixture has no auth story at all (GIT_HTTP_EXPORT_ALL, see New),
	// so explicitly opt each repo in.
	cfgCmd := exec.Command(gitBin, "config", "http.receivepack", "true")
	cfgCmd.Dir = repoPath
	if out, err := cfgCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("upstream: git config http.receivepack (%s): %w: %s", repoPath, err, out)
	}

	return repoPath, nil
}

func repoDirName(name string) string {
	if strings.HasSuffix(name, ".git") {
		return name
	}
	return name + ".git"
}

// resolveGitBin returns the real git binary to use when Options.GitBin (or
// InitBareRepo's gitBin) is unset: the conventional /usr/bin/git if
// present, else whatever `git` resolves to on PATH (the path taken by
// plain `go test`, run outside the e2e harness where PATH is not
// shadowed).
func resolveGitBin() string {
	if _, err := os.Stat(defaultGitBin); err == nil {
		return defaultGitBin
	}
	if p, err := exec.LookPath("git"); err == nil {
		return p
	}
	return defaultGitBin
}

// wellKnownGitExecDirs are the git-core ("exec-path") directories used by
// common Linux distributions' packaged git (Debian/Ubuntu's
// /usr/lib/git-core, some distros' /usr/libexec/git-core, and
// locally-built installs under /usr/local). Checking these first avoids an
// extra `git --exec-path` subprocess call in the common case; some
// environments' git wrappers (e.g. a policy-restricted broker-mediated git)
// reject less common flags like --exec-path outright even though they
// support the plain subcommands (init, config, clone, push, ...) this
// package actually needs to run.
var wellKnownGitExecDirs = []string{
	"/usr/lib/git-core",
	"/usr/libexec/git-core",
	"/usr/local/libexec/git-core",
	"/usr/local/lib/git-core",
}

// findGitHTTPBackend locates the `git-http-backend` CGI script, returning
// its full path and containing exec-path directory. It tries
// wellKnownGitExecDirs before falling back to asking gitBin itself via
// `--exec-path`.
func findGitHTTPBackend(gitBin string) (backend, execPath string, err error) {
	for _, dir := range wellKnownGitExecDirs {
		candidate := filepath.Join(dir, "git-http-backend")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, dir, nil
		}
	}

	out, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		return "", "", fmt.Errorf("upstream: git-http-backend not found in any of %v, and %s --exec-path failed: %w", wellKnownGitExecDirs, gitBin, err)
	}
	dir := strings.TrimSpace(string(out))
	candidate := filepath.Join(dir, "git-http-backend")
	if _, statErr := os.Stat(candidate); statErr != nil {
		return "", "", fmt.Errorf("upstream: git-http-backend not found at %s (from %s --exec-path): %w", candidate, gitBin, statErr)
	}
	return candidate, dir, nil
}
