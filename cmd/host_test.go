package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/version"
	"github.com/spf13/cobra"
)

// TestHostModeEnabled pins docs/plans/release-onboarding.md 決定2/PR5:
// host mode is unconditional now — BOID_MODE is gone, so there is no
// env-driven case matrix left to exercise here, just the always-true
// contract.
func TestHostModeEnabled(t *testing.T) {
	if !hostModeEnabled() {
		t.Error("hostModeEnabled() = false, want true (unconditional since PR5)")
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

// TestFindComposeRoot_DoesNotWalkUpFromCwd is the codex round-10 review of
// PR5, Blocker regression test: findComposeRoot used to walk up from cwd
// looking for scripts/deploy-container.sh, which became a drive-by
// code-execution vector once PR5 made host mode (and therefore this
// lookup) the unconditional default for every scope=remote command — a
// checkout an attacker controls, not necessarily boid's own, could plant
// its own scripts/deploy-container.sh and have it executed with the
// invoking user's own privileges the moment `boid` ran from inside (or
// below) it with the daemon not yet reachable. BOID_COMPOSE_ROOT must be
// the ONLY way to make this trust a checkout now; a real
// scripts/deploy-container.sh sitting in an ancestor directory must be
// ignored when it is unset.
func TestFindComposeRoot_DoesNotWalkUpFromCwd(t *testing.T) {
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

	if root, err := findComposeRoot(); err == nil {
		t.Fatalf("findComposeRoot() = %q, nil; want an error — BOID_COMPOSE_ROOT is unset, so an ancestor directory's own scripts/deploy-container.sh must NOT be trusted", root)
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
// decision logic (usableEngine/detectComposeEngine) be tested
// without them).
func writeFakeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// TestExtractComposeAssets_WritesEmbeddedContentByteForByte pins the
// round-2 codex review Major 1 fix's extraction step, extended by round-3
// (unifying the checkout and embedded-assets host-mode paths onto the SAME
// script): compose.yml, Dockerfile, AND deploy-container.sh, written into a
// tree rooted at hostModeAssetsDir(), must be byte-for-byte identical to
// the real files this checkout ships (build/container/{compose.yml,
// Dockerfile}, scripts/deploy-container.sh) — go:embed captured them at
// build time (build/container/assets.go, scripts/embed.go), this just
// proves the extraction round-trip doesn't corrupt/truncate/mismatch them.
// Also pins the layout itself: the returned root must mirror this repo's
// own directory structure (root/scripts/deploy-container.sh,
// root/build/container/{compose.yml,Dockerfile}) so the extracted script's
// own `dirname "${BASH_SOURCE[0]}"`-relative path computation resolves
// exactly as it would from a real checkout — and the script must be
// executable, since runDeployScript execs it directly.
func TestExtractComposeAssets_WritesEmbeddedContentByteForByte(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	root, err := extractComposeAssets()
	if err != nil {
		t.Fatalf("extractComposeAssets: %v", err)
	}

	wantCompose, err := os.ReadFile(filepath.Join("..", "build", "container", "compose.yml"))
	if err != nil {
		t.Fatalf("read real compose.yml: %v", err)
	}
	gotCompose, err := os.ReadFile(filepath.Join(root, "build", "container", "compose.yml"))
	if err != nil {
		t.Fatalf("read extracted compose.yml: %v", err)
	}
	if string(gotCompose) != string(wantCompose) {
		t.Error("extracted compose.yml does not match the real build/container/compose.yml byte-for-byte")
	}

	// compose.podman.override.yml (2026-07-26 dogfood): the deploy script
	// stacks this overlay on top of compose.yml when it detects rootless
	// podman. Missing from the extraction, the no-checkout fallback would
	// invoke a script whose `-f <root>/build/container/
	// compose.podman.override.yml` argument points at nothing — failing on
	// exactly the engine the overlay exists for, and only there.
	wantOverride, err := os.ReadFile(filepath.Join("..", "build", "container", "compose.podman.override.yml"))
	if err != nil {
		t.Fatalf("read real compose.podman.override.yml: %v", err)
	}
	gotOverride, err := os.ReadFile(filepath.Join(root, "build", "container", "compose.podman.override.yml"))
	if err != nil {
		t.Fatalf("read extracted compose.podman.override.yml: %v", err)
	}
	if string(gotOverride) != string(wantOverride) {
		t.Error("extracted compose.podman.override.yml does not match the real build/container/compose.podman.override.yml byte-for-byte")
	}

	wantDockerfile, err := os.ReadFile(filepath.Join("..", "build", "container", "Dockerfile"))
	if err != nil {
		t.Fatalf("read real Dockerfile: %v", err)
	}
	gotDockerfile, err := os.ReadFile(filepath.Join(root, "build", "container", "Dockerfile"))
	if err != nil {
		t.Fatalf("read extracted Dockerfile: %v", err)
	}
	if string(gotDockerfile) != string(wantDockerfile) {
		t.Error("extracted Dockerfile does not match the real build/container/Dockerfile byte-for-byte")
	}

	wantScript, err := os.ReadFile(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("read real deploy-container.sh: %v", err)
	}
	scriptPath := filepath.Join(root, "scripts", "deploy-container.sh")
	gotScript, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read extracted deploy-container.sh: %v", err)
	}
	if string(gotScript) != string(wantScript) {
		t.Error("extracted deploy-container.sh does not match the real scripts/deploy-container.sh byte-for-byte")
	}

	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat extracted deploy-container.sh: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("extracted deploy-container.sh mode = %o, want the owner-execute bit set", info.Mode().Perm())
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

// TestDetectComposeEngine_DockerWithoutComposePlugin_FallsBackToPodman
// pins the round-3 codex review Minor 1 fix: `docker version` succeeding
// is not proof the compose v2 plugin (`docker compose version`) is
// installed — a docker with a reachable engine but no compose plugin must
// not be preferred over a genuinely usable podman+podman-compose sitting
// right next to it.
func TestDetectComposeEngine_DockerWithoutComposePlugin_FallsBackToPodman(t *testing.T) {
	dir := t.TempDir()
	// `docker version` succeeds (engine reachable) but `docker compose
	// version` fails (no v2 plugin installed) — case-dispatch on $1 the
	// same way TestDeployFromEmbeddedAssets_NoImage_ClearError's fake
	// does.
	writeFakeExecutable(t, dir, "docker", `
case "$1" in
  version) exit 0 ;;
  *) exit 1 ;;
esac
`)
	writeFakeExecutable(t, dir, "podman", "exit 0")
	writeFakeExecutable(t, dir, "podman-compose", "exit 0")
	t.Setenv("PATH", dir)

	engine, composeCmd, err := detectComposeEngine(context.Background())
	if err != nil {
		t.Fatalf("detectComposeEngine: %v", err)
	}
	if engine != "podman" {
		t.Errorf("engine = %q, want podman (docker usable but missing the compose v2 plugin)", engine)
	}
	if len(composeCmd) != 1 || composeCmd[0] != "podman-compose" {
		t.Errorf("composeCmd = %v, want [podman-compose]", composeCmd)
	}
}

// TestDetectComposeEngine_DockerWithoutComposePlugin_NoPodman_Errors is the
// companion dead-end case: docker usable but no compose plugin, AND no
// podman at all — must error rather than silently picking a docker
// invocation that would fail at the actual `up` call.
func TestDetectComposeEngine_DockerWithoutComposePlugin_NoPodman_Errors(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", `
case "$1" in
  version) exit 0 ;;
  *) exit 1 ;;
esac
`)
	t.Setenv("PATH", dir)

	if _, _, err := detectComposeEngine(context.Background()); err == nil {
		t.Fatal("expected an error when docker lacks the compose plugin and no podman is present")
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

// TestWithHostModeLock_LivesBesideAssetsDir_NotConfigDir pins the round-3
// codex review Minor 2 fix: the flock file must live under
// hostModeAssetsDir() (XDG_STATE_HOME), not hostModeConfigDir()
// (XDG_CONFIG_HOME) as it did before this fix. Set to two DIFFERENT temp
// dirs here specifically to prove the lock path only depends on the
// state-home value — the concrete hazard this closes (withHostModeLock's
// own doc comment): two `boid` invocations with different XDG_CONFIG_HOME
// but the same XDG_STATE_HOME must take the SAME lock, or they gain no
// mutual exclusion at all despite both intending to serialize the same
// extraction/deploy race.
func TestWithHostModeLock_LivesBesideAssetsDir_NotConfigDir(t *testing.T) {
	configHome := t.TempDir()
	stateHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)

	ran := false
	if err := withHostModeLock(func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("withHostModeLock: %v", err)
	}
	if !ran {
		t.Fatal("withHostModeLock did not invoke fn")
	}

	assetsLock := filepath.Join(stateHome, "boid", "compose", cliLockFileName)
	if _, err := os.Stat(assetsLock); err != nil {
		t.Errorf("expected lock file at %s (beside the extracted assets, XDG_STATE_HOME-based): %v", assetsLock, err)
	}
	configLock := filepath.Join(configHome, "boid", cliLockFileName)
	if _, err := os.Stat(configLock); err == nil {
		t.Errorf("did not expect a lock file at %s (XDG_CONFIG_HOME-based) — the lock must live beside the assets it protects, not the cli-token's directory", configLock)
	}
}

// TestWaitForHealthy_StaleImage404_FailsFastWithClearError pins the
// round-3 codex review "Regression coverage" item 3: a daemon that answers
// HTTP at all but returns 404 for /api/cli-token-check specifically (a
// boid-runner image built before that endpoint existed) must fail
// IMMEDIATELY with a clear "image needs update" error, not silently retry
// for the full hostModeStartTimeout (5 minutes) on a condition that can
// never resolve on its own. The test itself finishing quickly (well under
// the suite's normal timeout) is part of what it pins — a regression back
// to the old "404 is just another kind of unhealthy" behavior would hang
// this test for 5 minutes.
func TestWaitForHealthy_StaleImage404_FailsFastWithClearError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every request 404s — the endpoint genuinely does not exist in
		// this "image", regardless of path/token, mirroring a pre-PR-3
		// boid-runner image that predates the CLI listener entirely.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	err := waitForHealthy(context.Background(), addr, "any-token")
	if err == nil {
		t.Fatal("expected an error for a 404 /api/cli-token-check response")
	}
	if !strings.Contains(err.Error(), "cli-token-check") {
		t.Errorf("error = %q, want it to mention the missing endpoint", err.Error())
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error = %q, want it to mention the stale image / need to rebuild", err.Error())
	}
}

// TestDeployFromEmbeddedAssets_RunsUnifiedScript_StartsComposeStack is the
// round-3 codex review Blocker 1 regression test, and the
// "Regression coverage" item 1 ("fresh deploy via embed path -> the compose
// stack actually starts") at the level this sandbox can actually exercise
// without a real docker/podman engine or root (no container can really be
// started here — see this repo's own e2e/run-container.sh for the real
// end-to-end verification, CLAUDE.md's "E2E テストは CI でしか検証できない").
//
// Before this fix, deployFromEmbeddedAssets ran a bare `compose up -d`
// directly instead of invoking scripts/deploy-container.sh at all. This
// test fakes the `docker` binary on PATH (logging every invocation's argv
// and the BOID_IMAGE env value to a file, always succeeding) and lets the
// REAL, embedded deploy-container.sh run end to end through
// deployFromEmbeddedAssets — proving the actual script (not a narrower
// Go-level reimplementation of it) runs, that no `docker build` is ever
// attempted (this path never passes `--build` — the embedded path can
// never build a fresh image, this file's own header comment), and that
// compose sees the BOID_IMAGE deployFromEmbeddedAssets computed via
// internal/version (docs/plans/release-onboarding.md 穴4/PR4's pull-first
// default).
//
// PR-4 (docs/plans/volume-only-daemon.md §論点e) removed
// deploy-container.sh's config-seed + effective-backend-validate
// `compose run --rm` steps entirely (container is the only sandbox backend
// now, selected unconditionally, not by a config.yaml key a fresh deploy
// needed to seed) — this test accordingly only asserts on the
// `compose up -d` invocation and the BOID_IMAGE propagation.
func TestDeployFromEmbeddedAssets_RunsUnifiedScript_StartsComposeStack(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker-invocations.log")

	writeFakeExecutable(t, dir, "docker", fmt.Sprintf(`
{
  echo "ARGS: $*"
  echo "BOID_IMAGE=${BOID_IMAGE:-unset}"
} >> %q
exit 0
`, logPath))
	// dir first (our fake docker must shadow any real one), then the real
	// PATH — unlike the other fakes-on-PATH tests in this file (which only
	// ever exec the fake directly), this one runs the REAL extracted
	// deploy-container.sh (a bash script, `#!/usr/bin/env bash` shebang)
	// end to end, so bash/mkdir/chown/id/getent/etc. must still resolve.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

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

	wantImage := version.DefaultContainerImage()

	if err := deployFromEmbeddedAssets(context.Background(), "good-token", addr); err != nil {
		t.Fatalf("deployFromEmbeddedAssets: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	log := string(logData)

	if !strings.Contains(log, "up -d") {
		t.Errorf("expected a `compose up -d` invocation; log:\n%s", log)
	}
	if strings.Contains(log, "ARGS: build") {
		t.Errorf("expected no `docker build` invocation under the embedded-assets path (no source tree to build from); log:\n%s", log)
	}
	if strings.Contains(log, "ARGS: --build") {
		t.Errorf("expected deployFromEmbeddedAssets to never pass --build to the script; log:\n%s", log)
	}
	// Only the actual compose-PROJECT invocations (`compose -f <file>
	// run/down/up`, deployFromEmbeddedAssets's own earlier
	// detectComposeEngine probe calls like bare `compose version` are
	// deliberately excluded via the "-f" match) spawned from WITHIN
	// deploy-container.sh need to have inherited BOID_IMAGE — the earlier
	// detectComposeEngine probe legitimately runs BEFORE runDeployScript
	// ever sets that env var at all, so its `version`/`compose version`
	// probes are expected to show BOID_IMAGE=unset.
	lines := strings.Split(log, "\n")
	found := false
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "ARGS: compose -f") {
			continue
		}
		found = true
		if want := "BOID_IMAGE=" + wantImage; lines[i+1] != want {
			t.Errorf("compose invocation %q ran with %q, want %q; log:\n%s", lines[i], lines[i+1], want, log)
		}
	}
	if !found {
		t.Errorf("expected at least one `compose -f ...` invocation; log:\n%s", log)
	}
}
