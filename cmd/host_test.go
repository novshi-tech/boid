package cmd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

func TestHostModeEnabled(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"", false},
		{"container", true},
		{"unix-socket", false},
		{"CONTAINER", false}, // case-sensitive, deliberately not tolerant
	}
	for _, c := range cases {
		t.Setenv(boidModeEnv, c.env)
		if got := hostModeEnabled(); got != c.want {
			t.Errorf("BOID_MODE=%q: hostModeEnabled() = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestIsRemoteScope(t *testing.T) {
	cases := []struct {
		scope string
		want  bool
	}{
		{scopeRemote, true},
		{scopeLocal, false},
		{scopeNeutral, false},
		{"", false},
	}
	for _, c := range cases {
		cmd := &cobra.Command{Annotations: map[string]string{scopeAnnotationKey: c.scope}}
		if got := isRemoteScope(cmd); got != c.want {
			t.Errorf("scope=%q: isRemoteScope() = %v, want %v", c.scope, got, c.want)
		}
	}
}

// TestLoadOrCreateCLIToken_GeneratesAndPersists pins the "generate once,
// persist" contract: a first call creates a non-empty token file; a
// second call against the same config dir returns the IDENTICAL value
// (atomicfile.PublishIfAbsent's own "first writer wins, read back" - not
// regenerated every call).
func TestLoadOrCreateCLIToken_GeneratesAndPersists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	first, err := loadOrCreateCLIToken()
	if err != nil {
		t.Fatalf("loadOrCreateCLIToken (first): %v", err)
	}
	if first == "" {
		t.Fatal("first token is empty")
	}

	second, err := loadOrCreateCLIToken()
	if err != nil {
		t.Fatalf("loadOrCreateCLIToken (second): %v", err)
	}
	if second != first {
		t.Errorf("second call returned a different token: first=%q second=%q, want identical (persist, not regenerate)", first, second)
	}
}

// TestLoadOrCreateCLIToken_FilePermissions pins the 0600 mode requirement
// — a shared secret must not be world/group readable.
func TestLoadOrCreateCLIToken_FilePermissions(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	if _, err := loadOrCreateCLIToken(); err != nil {
		t.Fatalf("loadOrCreateCLIToken: %v", err)
	}

	path := filepath.Join(configHome, "boid", cliTokenFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cli-token mode = %o, want 0600", perm)
	}
}

// TestLoadOrCreateCLIToken_TrimsTrailingNewline pins the round-2 codex
// review Minor 2 fix: a manually restored token file commonly carries a
// trailing newline (e.g. `echo $TOKEN > cli-token` instead of `printf`) —
// loadOrCreateCLIToken must trim it rather than handing the raw bytes
// (including the newline) straight into an `Authorization: Bearer
// <token>\n` header, which would fail deep inside net/http with a cryptic
// error nowhere near the actual mistake.
func TestLoadOrCreateCLIToken_TrimsTrailingNewline(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	dir := filepath.Join(configHome, "boid")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const want = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	if err := os.WriteFile(filepath.Join(dir, cliTokenFileName), []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	got, err := loadOrCreateCLIToken()
	if err != nil {
		t.Fatalf("loadOrCreateCLIToken: %v", err)
	}
	if got != want {
		t.Errorf("loadOrCreateCLIToken() = %q, want %q (trailing newline trimmed)", got, want)
	}
}

// TestLoadOrCreateCLIToken_TooShort_ClearError pins the format-validation
// half of Minor 2: an existing token file that is implausibly short (looks
// truncated/corrupt rather than a real generated token) must fail with a
// clear, actionable error naming the file — not silently be used as a
// Bearer credential that will just 401 forever with no hint why.
func TestLoadOrCreateCLIToken_TooShort_ClearError(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	dir := filepath.Join(configHome, "boid")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tokenPath := filepath.Join(dir, cliTokenFileName)
	if err := os.WriteFile(tokenPath, []byte("short\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	_, err := loadOrCreateCLIToken()
	if err == nil {
		t.Fatal("expected an error for an implausibly short token file")
	}
	if !strings.Contains(err.Error(), tokenPath) {
		t.Errorf("error = %q, want it to name the file %q", err.Error(), tokenPath)
	}
}

// TestLoadOrCreateCLIToken_EmbeddedWhitespace_ClearError pins the other
// corruption shape Minor 2 guards against: whitespace embedded anywhere in
// the token (not just a trailing newline) must also be rejected clearly.
func TestLoadOrCreateCLIToken_EmbeddedWhitespace_ClearError(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	dir := filepath.Join(configHome, "boid")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, cliTokenFileName), []byte("0123456789abcdef 0123456789abcdef0123456789abcdef01234\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	if _, err := loadOrCreateCLIToken(); err == nil {
		t.Fatal("expected an error for a token file with embedded whitespace")
	}
}

func TestFindComposeRoot_EnvOverride(t *testing.T) {
	t.Setenv("BOID_COMPOSE_ROOT", "/some/explicit/root")
	root, err := findComposeRoot()
	if err != nil {
		t.Fatalf("findComposeRoot: %v", err)
	}
	if root != "/some/explicit/root" {
		t.Errorf("root = %q, want %q", root, "/some/explicit/root")
	}
}

func TestFindComposeRoot_WalksUpFromCwd(t *testing.T) {
	t.Setenv("BOID_COMPOSE_ROOT", "")

	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "scripts", "deploy-container.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write deploy-container.sh: %v", err)
	}
	nested := filepath.Join(repoRoot, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(nested); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	root, err := findComposeRoot()
	if err != nil {
		t.Fatalf("findComposeRoot: %v", err)
	}
	// Resolve symlinks (e.g. macOS /tmp -> /private/tmp; harmless on
	// Linux too) before comparing, since os.Getwd can return either form.
	wantRoot, _ := filepath.EvalSymlinks(repoRoot)
	gotRoot, _ := filepath.EvalSymlinks(root)
	if gotRoot != wantRoot {
		t.Errorf("root = %q, want %q", gotRoot, wantRoot)
	}
}

func TestFindComposeRoot_NotFound(t *testing.T) {
	t.Setenv("BOID_COMPOSE_ROOT", "")

	dir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if _, err := findComposeRoot(); err == nil {
		t.Fatal("expected an error when no scripts/deploy-container.sh is found by walking up from cwd")
	}
}

// TestHostModeHealthy_CorrectToken_ReportsHealthy pins the round-2 codex
// review Blocker 2 fix: the readiness probe now hits the AUTHENTICATED
// /api/cli-token-check endpoint with the CONFIGURED token, not the public
// /api/health — so it actually proves the token in effect works, not just
// that some process is listening.
func TestHostModeHealthy_CorrectToken_ReportsHealthy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli-token-check" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	if !hostModeHealthy(context.Background(), addr, "good-token") {
		t.Error("expected hostModeHealthy() == true against a live /api/cli-token-check handler with the correct token")
	}
}

// TestHostModeHealthy_WrongToken_ReportsUnhealthy is the direct regression
// test for the concrete Blocker 2 failure: a daemon container still running
// with a STALE BOID_CLI_TOKEN (e.g. ~/.config/boid/cli-token was
// deleted/regenerated while the container kept running) must be reported
// unhealthy — /api/health alone would have returned 200 here and left every
// real request 401ing forever.
func TestHostModeHealthy_WrongToken_ReportsUnhealthy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli-token-check" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer the-real-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	if hostModeHealthy(context.Background(), addr, "stale-token") {
		t.Error("expected hostModeHealthy() == false when the configured token does not match what the daemon holds")
	}
}

func TestHostModeHealthy_Unreachable(t *testing.T) {
	// Nothing listens on this port (0 lets the OS pick one, then we close
	// immediately, so the port is very likely free but definitely not
	// serving anything by the time we probe it).
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if hostModeHealthy(context.Background(), addr, "any-token") {
		t.Error("expected hostModeHealthy() == false against a closed port")
	}
}

// TestEnsureHostModeDaemon_NoAutostart_FailsFastWithoutDeploying pins the
// round-2 codex review Major 3 fix: container autostart used to ignore the
// existing global BOID_NO_AUTOSTART=1 contract entirely (internal/client.
// NoAutostartEnv — honored by the bare-metal EnsureRunningAt path already).
// With the target unreachable and BOID_NO_AUTOSTART=1 set, this must return
// a clear error WITHOUT ever attempting to locate/invoke
// scripts/deploy-container.sh — pinned here by deliberately leaving
// BOID_COMPOSE_ROOT unset and cwd outside any checkout, so if the
// autostart-skip check were missing, the very next thing attempted
// (findComposeRoot) would fail with a DIFFERENT error message than the one
// asserted below.
func TestEnsureHostModeDaemon_NoAutostart_FailsFastWithoutDeploying(t *testing.T) {
	t.Setenv(client.NoAutostartEnv, "1")
	t.Setenv("BOID_COMPOSE_ROOT", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	dir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Nothing listens here — the daemon is "not running".
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	err = ensureHostModeDaemon(context.Background(), addr, "some-token")
	if err == nil {
		t.Fatal("expected an error when the daemon is unreachable and BOID_NO_AUTOSTART=1")
	}
	if !strings.Contains(err.Error(), client.NoAutostartEnv) {
		t.Errorf("error = %q, want it to mention %s", err.Error(), client.NoAutostartEnv)
	}
	if strings.Contains(err.Error(), "could not locate") {
		t.Errorf("error = %q, must not be the findComposeRoot error — autostart-skip should short-circuit before ever trying to locate the deploy script", err.Error())
	}
}

// writeFakeExecutable writes an executable shell script named name into
// dir, containing body — used below to stub docker/podman/podman-compose
// on PATH without needing the real engines installed (this sandbox has
// neither, per CLAUDE.md's "sqlite 依存パッケージは sandbox 内でビルド不可"
// note and general no-docker constraint — these fakes let the pure
// decision logic (usableEngine/detectComposeEngine/imageExists) be tested
// without them).
func writeFakeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// TestExtractComposeAssets_WritesEmbeddedContentByteForByte pins the
// round-2 codex review Major 1 fix's extraction step: compose.yml and
// Dockerfile written into hostModeAssetsDir() must be byte-for-byte
// identical to the real files this checkout ships (build/container/
// {compose.yml,Dockerfile}) — go:embed captured them at build time
// (build/container/assets.go), this just proves the extraction round-trip
// doesn't corrupt/truncate/mismatch them.
func TestExtractComposeAssets_WritesEmbeddedContentByteForByte(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir, err := extractComposeAssets()
	if err != nil {
		t.Fatalf("extractComposeAssets: %v", err)
	}

	wantCompose, err := os.ReadFile(filepath.Join("..", "build", "container", "compose.yml"))
	if err != nil {
		t.Fatalf("read real compose.yml: %v", err)
	}
	gotCompose, err := os.ReadFile(filepath.Join(dir, "compose.yml"))
	if err != nil {
		t.Fatalf("read extracted compose.yml: %v", err)
	}
	if string(gotCompose) != string(wantCompose) {
		t.Error("extracted compose.yml does not match the real build/container/compose.yml byte-for-byte")
	}

	wantDockerfile, err := os.ReadFile(filepath.Join("..", "build", "container", "Dockerfile"))
	if err != nil {
		t.Fatalf("read real Dockerfile: %v", err)
	}
	gotDockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read extracted Dockerfile: %v", err)
	}
	if string(gotDockerfile) != string(wantDockerfile) {
		t.Error("extracted Dockerfile does not match the real build/container/Dockerfile byte-for-byte")
	}
}

func TestUsableEngine_TrueForWorkingCommand(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "exit 0")
	t.Setenv("PATH", dir)

	if !usableEngine(context.Background(), "docker") {
		t.Error("expected usableEngine(docker) == true when `docker version` exits 0")
	}
}

func TestUsableEngine_FalseWhenVersionFails(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "exit 1")
	t.Setenv("PATH", dir)

	if usableEngine(context.Background(), "docker") {
		t.Error("expected usableEngine(docker) == false when `docker version` exits non-zero (present but unusable)")
	}
}

func TestUsableEngine_FalseWhenNotOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir, nothing on PATH
	if usableEngine(context.Background(), "docker") {
		t.Error("expected usableEngine(docker) == false when not on PATH at all")
	}
}

// TestDetectComposeEngine_PrefersUsableDockerOverUnusableDocker pins the
// Major 4-mirroring intent: a `docker` on PATH that fails `docker version`
// must not be selected, but this test also confirms the simpler positive
// case — a working docker is picked and reported as "docker compose".
func TestDetectComposeEngine_PicksWorkingDocker(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "exit 0")
	t.Setenv("PATH", dir)

	engine, composeCmd, err := detectComposeEngine(context.Background())
	if err != nil {
		t.Fatalf("detectComposeEngine: %v", err)
	}
	if engine != "docker" {
		t.Errorf("engine = %q, want docker", engine)
	}
	if len(composeCmd) != 2 || composeCmd[0] != "docker" || composeCmd[1] != "compose" {
		t.Errorf("composeCmd = %v, want [docker compose]", composeCmd)
	}
}

func TestDetectComposeEngine_FallsBackToPodmanWhenDockerUnusable(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "exit 1") // present but broken
	writeFakeExecutable(t, dir, "podman", "exit 0")
	writeFakeExecutable(t, dir, "podman-compose", "exit 0")
	t.Setenv("PATH", dir)

	engine, composeCmd, err := detectComposeEngine(context.Background())
	if err != nil {
		t.Fatalf("detectComposeEngine: %v", err)
	}
	if engine != "podman" {
		t.Errorf("engine = %q, want podman (docker present but unusable)", engine)
	}
	if len(composeCmd) != 1 || composeCmd[0] != "podman-compose" {
		t.Errorf("composeCmd = %v, want [podman-compose]", composeCmd)
	}
}

func TestDetectComposeEngine_PodmanWithoutPodmanCompose_Errors(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "podman", "exit 0")
	t.Setenv("PATH", dir)

	if _, _, err := detectComposeEngine(context.Background()); err == nil {
		t.Fatal("expected an error when podman is usable but podman-compose is missing")
	}
}

func TestDetectComposeEngine_NoEngineAtAll_Errors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, _, err := detectComposeEngine(context.Background()); err == nil {
		t.Fatal("expected an error when neither engine is present")
	}
}

func TestImageExists_TrueWhenInspectSucceeds(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "exit 0")
	t.Setenv("PATH", dir)

	if !imageExists(context.Background(), "docker", composeImageTag) {
		t.Error("expected imageExists == true when `docker image inspect` exits 0")
	}
}

func TestImageExists_FalseWhenInspectFails(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "exit 1")
	t.Setenv("PATH", dir)

	if imageExists(context.Background(), "docker", composeImageTag) {
		t.Error("expected imageExists == false when `docker image inspect` exits non-zero (no such image)")
	}
}

// TestDeployFromEmbeddedAssets_NoEngine_ClearError pins the round-2 codex
// review Major 1 fallback's dead-end error path: no checkout AND no usable
// engine at all must fail clearly rather than attempting anything.
func TestDeployFromEmbeddedAssets_NoEngine_ClearError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	err := deployFromEmbeddedAssets(context.Background(), "some-token", "127.0.0.1:8442")
	if err == nil {
		t.Fatal("expected an error when no engine is usable at all")
	}
	if !strings.Contains(err.Error(), "no boid repo checkout found") {
		t.Errorf("error = %q, want it to mention the missing checkout", err.Error())
	}
}

// TestDeployFromEmbeddedAssets_NoImage_ClearError pins the concrete Major 1
// failure this fallback exists to improve: an engine IS usable, but
// composeImageTag was never built (no checkout, ever, on this host) — must
// fail with a specific, actionable message rather than a raw `compose up`
// error or (pre-fix) the unrelated "could not locate deploy-container.sh"
// error.
func TestDeployFromEmbeddedAssets_NoImage_ClearError(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", "exit 0") // `docker version` and `docker image inspect` both "succeed" (exit 0)... but see below
	t.Setenv("PATH", dir)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Rewrite the fake so `version` succeeds but `image inspect` fails —
	// distinguishing the two subcommands the way a real docker with no
	// matching image locally would.
	writeFakeExecutable(t, dir, "docker", `
case "$1" in
  version) exit 0 ;;
  image) exit 1 ;;
  *) exit 1 ;;
esac
`)

	err := deployFromEmbeddedAssets(context.Background(), "some-token", "127.0.0.1:8442")
	if err == nil {
		t.Fatal("expected an error when the image does not exist locally")
	}
	if !strings.Contains(err.Error(), composeImageTag) {
		t.Errorf("error = %q, want it to name the missing image %q", err.Error(), composeImageTag)
	}
}

func TestFilterEnv(t *testing.T) {
	env := []string{"PATH=/usr/bin", "BOID_CLI_TOKEN=stale-value", "HOME=/home/x"}
	got := filterEnv(env, "BOID_CLI_TOKEN")
	want := []string{"PATH=/usr/bin", "HOME=/home/x"}
	if len(got) != len(want) {
		t.Fatalf("filterEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filterEnv()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFilterEnv_NoMatch(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/home/x"}
	got := filterEnv(env, "BOID_CLI_TOKEN")
	if len(got) != 2 {
		t.Errorf("filterEnv() with no match = %v, want unchanged", got)
	}
}
