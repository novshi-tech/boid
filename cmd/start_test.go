package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
)

func TestDefaultAllowedDomains_IncludeCodexDomains(t *testing.T) {
	got := make(map[string]struct{})
	for _, domain := range defaultAllowedDomains() {
		got[domain] = struct{}{}
	}

	for _, domain := range []string{"api.openai.com", "auth.openai.com", "chatgpt.com", ".claude.com", ".models.dev"} {
		if _, ok := got[domain]; !ok {
			t.Fatalf("defaultAllowedDomains() missing %q", domain)
		}
	}
}

// TestRefuseRootUID pins the codex-review Blocker fix for PR2
// (docs/plans/release-onboarding.md 決定1): `boid start` must refuse to
// run as uid 0 regardless of how it got there (BOID_UID=0 in compose.yml,
// deploy-container.sh run as root, ...) — a root daemon would otherwise
// pass os.Getuid()==0 straight through internal/server/wire.go into
// dispatcher.NewContainerBackend's own uid-0 guard, silently falling back
// to a uid that does not own the workspace HOME a root daemon created.
func TestRefuseRootUID(t *testing.T) {
	if err := refuseRootUID(0); err == nil {
		t.Error("refuseRootUID(0) = nil, want an error rejecting uid 0")
	}
	for _, uid := range []int{1, 1000, 65534} {
		if err := refuseRootUID(uid); err != nil {
			t.Errorf("refuseRootUID(%d) = %v, want nil (non-root uid must be accepted)", uid, err)
		}
	}
}

func TestBuildStartConfig_UsesDefaults(t *testing.T) {
	dataHome := t.TempDir()
	socketPath := filepath.Join(t.TempDir(), "boid.sock")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("BOID_SOCKET", socketPath)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}

	wantDataDir := filepath.Join(dataHome, "boid")
	if cfg.DBPath != filepath.Join(wantDataDir, "boid.db") {
		t.Fatalf("DBPath = %q, want %q", cfg.DBPath, filepath.Join(wantDataDir, "boid.db"))
	}
	if cfg.KitsDir != filepath.Join(wantDataDir, "kits") {
		t.Fatalf("KitsDir = %q, want %q", cfg.KitsDir, filepath.Join(wantDataDir, "kits"))
	}
	if cfg.SocketPath != socketPath {
		t.Fatalf("SocketPath = %q, want %q", cfg.SocketPath, socketPath)
	}
	if cfg.HTTPAddr != defaultStartHTTPAddr {
		t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, defaultStartHTTPAddr)
	}
	if cfg.KeyFilePath != filepath.Join(wantDataDir, "secret.key") {
		t.Fatalf("KeyFilePath = %q, want %q", cfg.KeyFilePath, filepath.Join(wantDataDir, "secret.key"))
	}
	if cfg.TLSDir != filepath.Join(wantDataDir, "tls") {
		t.Fatalf("TLSDir = %q, want %q", cfg.TLSDir, filepath.Join(wantDataDir, "tls"))
	}
	if len(cfg.AllowedDomains) == 0 {
		t.Fatal("AllowedDomains should not be empty")
	}
	// PR-3 Option 4 host-mode redesign (docs/plans/volume-only-daemon.md
	// §論点c): CLIAddr defaults to client.DefaultCLIAddr() ("127.0.0.1:8442")
	// when no --cli-addr override is given, exactly like every other
	// opts.* field above.
	if cfg.CLIAddr != "127.0.0.1:8442" {
		t.Fatalf("CLIAddr = %q, want %q", cfg.CLIAddr, "127.0.0.1:8442")
	}
	if cfg.CLIToken != "" {
		t.Fatalf("CLIToken = %q, want empty (BOID_CLI_TOKEN not set)", cfg.CLIToken)
	}
}

// TestBuildStartConfig_LogLevelFromConfig pins that config.yaml's
// `log.level` flows through to server.Config.LogLevel unchanged — the value
// cmd/start.go's runDaemonChild later passes to
// internal/daemon.ApplyLogLevel. Unset (no config.yaml at all, the default
// TestBuildStartConfig_UsesDefaults case) leaves LogLevel empty.
func TestBuildStartConfig_LogLevelFromConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "boid"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configHome, "boid", "config.yaml")
	if err := os.WriteFile(configPath, []byte("log:\n  level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
}

// TestBuildStartConfig_LogLevelUnset_Empty pins the default: no `log:` block
// in config.yaml (the common case, and every pre-log.level config.yaml)
// leaves cfg.LogLevel empty, the no-op internal/daemon.ApplyLogLevel treats
// as "leave slog's built-in default (info) alone."
func TestBuildStartConfig_LogLevelUnset_Empty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	if cfg.LogLevel != "" {
		t.Fatalf("LogLevel = %q, want empty", cfg.LogLevel)
	}
}

// withFakeContainerSentinel temporarily points containerSentinelPaths at a
// single file the test creates (or does not create), standing in for the
// real /.dockerenv / /run/.containerenv this process cannot fake without
// root — restores the real paths afterward so this global does not leak
// across other tests in the same binary.
func withFakeContainerSentinel(t *testing.T, present bool) {
	t.Helper()
	orig := containerSentinelPaths
	t.Cleanup(func() { containerSentinelPaths = orig })

	sentinel := filepath.Join(t.TempDir(), "dockerenv")
	if present {
		if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
			t.Fatalf("write fake sentinel: %v", err)
		}
	}
	containerSentinelPaths = []string{sentinel}
}

// TestBuildStartConfig_ContainerMode_DefaultsHTTPAddrToAllInterfaces pins
// 穴 8 (a) (docs/plans/release-onboarding.md): under
// build/container/compose.yml, a fresh boid_state volume has no
// web.http_addr in config.yaml, so buildStartConfig used to fall back to
// defaultStartHTTPAddr ("127.0.0.1:8080") — binding the container's own
// loopback, which compose's port publish cannot make reachable from the
// host at all. runningUnderComposeContainer requires BOTH
// BOID_LOG_STDOUT=1 (compose sets this) AND a real container-runtime
// sentinel file (see that function's own doc comment for why BOID_LOG_STDOUT
// alone is not a safe trigger, codex round-6 review): with both signals
// present, the fallback address becomes 0.0.0.0:8080 instead.
func TestBuildStartConfig_ContainerMode_DefaultsHTTPAddrToAllInterfaces(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOID_LOG_STDOUT", "1")
	withFakeContainerSentinel(t, true)

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	if cfg.HTTPAddr != "0.0.0.0:8080" {
		t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, "0.0.0.0:8080")
	}
}

// TestBuildStartConfig_LogStdoutAlone_DoesNotFlipToAllInterfaces pins the
// Blocker fix (codex round-6 review): an earlier revision used
// BOID_LOG_STDOUT=1 alone as the "am I in a container" signal, but that
// flag's own contract (daemon.ShouldLogToStdout's doc comment) is "a
// supervisor already owns my stdout/session lifecycle" — a BARE HOST
// systemd unit could set it too, for reasons having nothing to do with
// containers, and would then unexpectedly flip a previously
// loopback-only Web UI listener to every host interface. With
// BOID_LOG_STDOUT=1 set but NO container sentinel file present, the
// fallback must stay at the loopback-only default.
func TestBuildStartConfig_LogStdoutAlone_DoesNotFlipToAllInterfaces(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOID_LOG_STDOUT", "1")
	withFakeContainerSentinel(t, false)

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	if cfg.HTTPAddr != defaultStartHTTPAddr {
		t.Fatalf("HTTPAddr = %q, want the bare-host default %q", cfg.HTTPAddr, defaultStartHTTPAddr)
	}
}

// TestBuildStartConfig_ContainerMode_ConfigYAMLStillWins pins that an
// explicit web.http_addr in config.yaml (e.g. set via `boid config set` per
// 穴 8's documented recovery path) still overrides the container-mode
// fallback — runningUnderComposeContainer only ever supplies a different
// FALLBACK default, it must not shadow a value the user (or a previous
// `boid web set-addr`/`config set` run) already persisted.
func TestBuildStartConfig_ContainerMode_ConfigYAMLStillWins(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("BOID_LOG_STDOUT", "1")
	withFakeContainerSentinel(t, true)
	if err := os.MkdirAll(filepath.Join(configHome, "boid"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configHome, "boid", "config.yaml")
	if err := os.WriteFile(configPath, []byte("web:\n  http_addr: 10.0.0.5:9090\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	if cfg.HTTPAddr != "10.0.0.5:9090" {
		t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, "10.0.0.5:9090")
	}
}

// TestBuildStartConfig_LogLevelInvalid_Rejected pins that an unrecognized
// log.level value fails buildStartConfig outright (via config.Load's own
// Config.UnmarshalYAML validation) — the same "config.Load()'s error is
// buildStartConfig's error too" contract every other config.yaml validation
// failure already gets here (e.g. an invalid gc.interval).
func TestBuildStartConfig_LogLevelInvalid_Rejected(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "boid"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configHome, "boid", "config.yaml")
	if err := os.WriteFile(configPath, []byte("log:\n  level: verbose\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := buildStartConfig(startConfigOptions{}); err == nil {
		t.Fatal("expected buildStartConfig() to fail for an invalid log.level, got nil error")
	}
}

// TestBuildStartConfig_CLIAddrOverride pins startConfigOptions.CLIAddr
// (--cli-addr) taking precedence over client.DefaultCLIAddr(), mirroring
// TestBuildStartConfig_UsesOverrides' own coverage of the other opts.*
// fields.
func TestBuildStartConfig_CLIAddrOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := buildStartConfig(startConfigOptions{CLIAddr: "127.0.0.1:19999"})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	if cfg.CLIAddr != "127.0.0.1:19999" {
		t.Fatalf("CLIAddr = %q, want %q", cfg.CLIAddr, "127.0.0.1:19999")
	}
}

// TestBuildStartConfig_CLITokenFromEnv pins BOID_CLI_TOKEN -> cfg.CLIToken
// (PR-3 Option 4 host-mode redesign): `boid`'s own host-mode orchestration
// (cmd/host.go) passes the shared secret to the daemon container via this
// env var, and buildStartConfig is the only place that reads it.
func TestBuildStartConfig_CLITokenFromEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOID_CLI_TOKEN", "test-token-value")

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	if cfg.CLIToken != "test-token-value" {
		t.Fatalf("CLIToken = %q, want %q", cfg.CLIToken, "test-token-value")
	}
}

// TestShouldRunForeground pins Major 6 (PR6 codex review): the double-fork
// suppression decision must route through a single shared seam reachable
// from either --foreground (the primary, discoverable path for any process
// supervisor) or BOID_DAEMON_CHILD=1 (daemon.IsChild() — kept for
// build/container/compose.yml's existing config), and either one alone
// must be sufficient — a supervisor should never need to set both.
func TestShouldRunForeground(t *testing.T) {
	cases := []struct {
		name string
		flag bool
		env  string
		want bool
	}{
		{"neither set: double-fork (default host behavior)", false, "", false},
		{"--foreground alone", true, "", true},
		{"BOID_DAEMON_CHILD=1 alone", false, "1", true},
		{"both set", true, "1", true},
		{"BOID_DAEMON_CHILD set to something other than 1", false, "0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.env == "" {
				t.Setenv("BOID_DAEMON_CHILD", "")
			} else {
				t.Setenv("BOID_DAEMON_CHILD", c.env)
			}
			if got := shouldRunForeground(c.flag); got != c.want {
				t.Errorf("shouldRunForeground(%v) with BOID_DAEMON_CHILD=%q = %v, want %v", c.flag, c.env, got, c.want)
			}
		})
	}
}

func TestBuildStartConfig_UsesOverrides(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := buildStartConfig(startConfigOptions{
		DBPath:      "/tmp/custom.db",
		SocketPath:  "/tmp/custom.sock",
		KitsDir:     "/tmp/kits",
		KeyFilePath: "/tmp/boid.key",
	})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}

	if cfg.DBPath != "/tmp/custom.db" {
		t.Fatalf("DBPath = %q, want %q", cfg.DBPath, "/tmp/custom.db")
	}
	if cfg.SocketPath != "/tmp/custom.sock" {
		t.Fatalf("SocketPath = %q, want %q", cfg.SocketPath, "/tmp/custom.sock")
	}
	if cfg.KitsDir != "/tmp/kits" {
		t.Fatalf("KitsDir = %q, want %q", cfg.KitsDir, "/tmp/kits")
	}
	if cfg.KeyFilePath != "/tmp/boid.key" {
		t.Fatalf("KeyFilePath = %q, want %q", cfg.KeyFilePath, "/tmp/boid.key")
	}
}

// TestRunComposeUp_NoCheckout_IgnoresNoAutostart_StartsComposeStack pins
// docs/plans/release-onboarding.md 決定2/PR5's redefinition of `boid
// start`: runComposeUp must run the deploy script's up cycle
// UNCONDITIONALLY, even with BOID_NO_AUTOSTART=1 set — that knob exists to
// stop an UNRELATED scope=remote command (e.g. `boid task list`) from
// silently autostarting a daemon as a side effect (cmd/host.go's
// ensureHostModeDaemon), not to make an EXPLICIT `boid start` itself a
// no-op. Uses the same fake-docker-on-PATH + real-embedded-script
// technique as cmd/host_test.go's
// TestDeployFromEmbeddedAssets_RunsUnifiedScript_StartsComposeStack (no
// real checkout on the fake PATH/cwd, so this exercises the
// no-checkout/embedded-assets branch of runComposeUp's own
// findComposeRoot-or-embedded dispatch).
func TestRunComposeUp_NoCheckout_IgnoresNoAutostart_StartsComposeStack(t *testing.T) {
	t.Setenv(client.NoAutostartEnv, "1")

	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker-invocations.log")
	writeFakeExecutable(t, dir, "docker", fmt.Sprintf(`
{
  echo "ARGS: $*"
} >> %q
exit 0
`, logPath))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// No real checkout reachable: pin BOID_COMPOSE_ROOT empty and run from
	// a bare temp dir, so findComposeRoot fails and runComposeUp falls
	// through to deployFromEmbeddedAssets — same setup as
	// TestEnsureHostModeDaemon_NoAutostart_FailsFastWithoutDeploying in
	// cmd/host_test.go.
	t.Setenv("BOID_COMPOSE_ROOT", "")
	cwd := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli-token-check" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	if err := runComposeUp(context.Background(), ts.Listener.Addr().String()); err != nil {
		t.Fatalf("runComposeUp: %v (BOID_NO_AUTOSTART=1 must not block an explicit `boid start`)", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	if !strings.Contains(string(logData), "up -d") {
		t.Errorf("expected a `compose up -d` invocation despite BOID_NO_AUTOSTART=1; log:\n%s", string(logData))
	}
}

// TestRunComposeDownScript_InvokesDeployScriptWithDown pins `boid stop`'s
// redefinition (cmd/stop.go, docs/plans/release-onboarding.md 決定2/PR5):
// it must invoke scripts/deploy-container.sh with exactly `--down`, not
// reimplement `compose down` directly — a fake deploy-container.sh here
// (not the real embedded one; --down's own engine-detection/podman-overlay
// logic is exercised at the shell-script level, not from Go) just records
// its own argv to prove the wiring.
func TestRunComposeDownScript_InvokesDeployScriptWithDown(t *testing.T) {
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "invocation.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"ARGS: $*\" >> %q\nexit 0\n", logPath)
	scriptPath := filepath.Join(scriptsDir, "deploy-container.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runComposeDownScript(context.Background(), root); err != nil {
		t.Fatalf("runComposeDownScript: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	if strings.TrimSpace(string(logData)) != "ARGS: --down" {
		t.Errorf("invocation log = %q, want %q", strings.TrimSpace(string(logData)), "ARGS: --down")
	}
}

// TestDeployContainerScript_Down_SkipsPodmanSocketPreflight is the codex
// round-1 review of PR5, Major 4 regression: scripts/deploy-container.sh's
// own podman.socket active-check preflight used to run unconditionally,
// even for --down — meaning `boid stop` could never recover from EXACTLY
// the failure mode that check exists to catch (a stopped/never-enabled
// podman.socket), since the preflight refused before ever reaching the
// down logic. Runs the REAL script (not a fake) with fake podman/
// podman-compose/systemctl on PATH — systemctl always reports inactive —
// and asserts --down still succeeds while a plain (up) invocation under
// the identical fake environment still correctly refuses.
func TestDeployContainerScript_Down_SkipsPodmanSocketPreflight(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("real deploy-container.sh not found at %s: %v", scriptPath, err)
	}

	dir := t.TempDir()
	writeFakeExecutable(t, dir, "systemctl", `exit 3`) // "is-active" always reports inactive
	writeFakeExecutable(t, dir, "podman", `
case "$1" in
  version) exit 0 ;;
  info) echo "false"; exit 0 ;;
  *) exit 0 ;;
esac
`)
	writeFakeExecutable(t, dir, "podman-compose", `exit 0`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	downCmd := exec.Command(scriptPath, "--down")
	downOut, downErr := downCmd.CombinedOutput()
	if downErr != nil {
		t.Fatalf("scripts/deploy-container.sh --down failed with fake-inactive podman.socket: %v\noutput:\n%s", downErr, downOut)
	}
	if strings.Contains(string(downOut), "podman.socket is not active") {
		t.Errorf("--down must not run the podman.socket preflight at all; output:\n%s", downOut)
	}

	upCmd := exec.Command(scriptPath)
	upOut, upErr := upCmd.CombinedOutput()
	if upErr == nil {
		t.Fatal("expected a plain (up) invocation to still refuse with an inactive podman.socket")
	}
	if !strings.Contains(string(upOut), "podman.socket is not active") {
		t.Errorf("expected the podman.socket preflight error for a plain (up) invocation; output:\n%s", upOut)
	}
}
