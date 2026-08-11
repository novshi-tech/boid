//go:build linux

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
	"github.com/spf13/cobra"
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

// TestDefaultAllowedDomains_IncludeNodeDistribution pins the fix for the
// 2026-08-11 dogfood failure: a repo pinning a node version via volta
// (package.json's "volta": {"node": "..."}) or via .nvmrc cannot run any
// node/npm/pnpm command at all when the pinned version differs from the
// one baked into the runner image — volta downloads the missing toolchain
// from nodejs.org, and the egress proxy answered CONNECT with 403 because
// the floor carried registry.npmjs.org but not the distribution host.
func TestDefaultAllowedDomains_IncludeNodeDistribution(t *testing.T) {
	got := make(map[string]struct{})
	for _, domain := range defaultAllowedDomains() {
		got[domain] = struct{}{}
	}

	if _, ok := got["nodejs.org"]; !ok {
		t.Errorf("defaultAllowedDomains() missing %q — volta/nvm cannot fetch a pinned node toolchain without it", "nodejs.org")
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

// TestEffectiveBoidUID_PrefersBOID_UIDEnv is the codex round-9 review of
// PR5, Major regression test: a non-root CLI process's own os.Getuid()
// must NOT be what refuseRootUID checks when BOID_UID is explicitly set
// to something else — compose's `user: "${BOID_UID:-1000}:0"` substitutes
// BOID_UID at compose-parse time on the HOST (not as a container env
// var), so a non-root operator exporting BOID_UID=0 changes what uid the
// CONTAINER runs as without changing this process's own os.Getuid() at
// all. effectiveBoidUID() must reflect BOID_UID when set, so
// refuseRootUID(effectiveBoidUID()) actually catches that case.
func TestEffectiveBoidUID_PrefersBOID_UIDEnv(t *testing.T) {
	t.Setenv("BOID_UID", "")
	if got := effectiveBoidUID(); got != os.Getuid() {
		t.Errorf("effectiveBoidUID() with BOID_UID unset = %d, want os.Getuid() = %d", got, os.Getuid())
	}

	t.Setenv("BOID_UID", "0")
	if got := effectiveBoidUID(); got != 0 {
		t.Errorf("effectiveBoidUID() with BOID_UID=0 = %d, want 0", got)
	}
	if err := refuseRootUID(effectiveBoidUID()); err == nil {
		t.Error("refuseRootUID(effectiveBoidUID()) with BOID_UID=0 must refuse, even though this test process itself is not root")
	}

	t.Setenv("BOID_UID", "1234")
	if got := effectiveBoidUID(); got != 1234 {
		t.Errorf("effectiveBoidUID() with BOID_UID=1234 = %d, want 1234", got)
	}

	// An unparseable BOID_UID falls back to os.Getuid() rather than
	// erroring here — compose/docker will surface their own clear error
	// against the malformed value when they try to use it.
	t.Setenv("BOID_UID", "not-a-number")
	if got := effectiveBoidUID(); got != os.Getuid() {
		t.Errorf("effectiveBoidUID() with unparseable BOID_UID = %d, want os.Getuid() fallback = %d", got, os.Getuid())
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

// TestBuildStartConfig_ServicesFloorFromConfig pins that config.yaml's
// `services_floor` flows through to server.Config.ServicesFloor unchanged —
// the same "captured once at daemon startup, ReloadRestartRequired" pattern
// AllowedDomains already has (docs/plans/api-gateway.md §3, mirroring
// sandbox.allowed_domains' own floor). This is the one hop in the config.yaml
// services_floor -> Config.ServicesFloor -> dispatcher.WireConfig.
// APIGatewayServicesFloor -> Runner.APIGatewayServicesFloor chain that isn't
// already covered by internal/config's own parse tests or
// internal/dispatcher's resolveEnabledAPIServices tests.
func TestBuildStartConfig_ServicesFloorFromConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "boid"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configHome, "boid", "config.yaml")
	if err := os.WriteFile(configPath, []byte("services_floor:\n  - myapp\n  - bitbucket-api\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	want := []string{"myapp", "bitbucket-api"}
	if len(cfg.ServicesFloor) != len(want) {
		t.Fatalf("ServicesFloor = %v, want %v", cfg.ServicesFloor, want)
	}
	for i, v := range want {
		if cfg.ServicesFloor[i] != v {
			t.Fatalf("ServicesFloor = %v, want %v", cfg.ServicesFloor, want)
		}
	}
}

// TestBuildStartConfig_ServicesFloorUnset_Empty pins the default: no
// `services_floor:` key in config.yaml leaves cfg.ServicesFloor empty rather
// than nil-panicking or defaulting to something surprising.
func TestBuildStartConfig_ServicesFloorUnset_Empty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig() error = %v", err)
	}
	if len(cfg.ServicesFloor) != 0 {
		t.Fatalf("ServicesFloor = %v, want empty", cfg.ServicesFloor)
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

	var out strings.Builder
	if err := runComposeUp(context.Background(), ts.Listener.Addr().String(), &out); err != nil {
		t.Fatalf("runComposeUp: %v (BOID_NO_AUTOSTART=1 must not block an explicit `boid start`)", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	if !strings.Contains(string(logData), "up -d") {
		t.Errorf("expected a `compose up -d` invocation despite BOID_NO_AUTOSTART=1; log:\n%s", string(logData))
	}

	// docs/plans/release-onboarding.md 目標オンボーディングフロー: `boid
	// start` must print the "next steps" onboarding guidance (pair the Web
	// UI, register a project, set an init script, sign into the agent) on
	// success — see printNextStepsGuidance's own doc comment for why this
	// is the step new users most often get stuck on.
	for _, want := range []string{"boid web pair", "boid project add", "boid workspace set-init-script", "boid agent claude"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("runComposeUp output missing next-steps guidance %q; got:\n%s", want, out.String())
		}
	}
}

// TestPrintNextStepsGuidance_MentionsInteractiveAgentLoginIsTheHardPart
// pins the specific "this is where new users get stuck" framing
// docs/plans/release-onboarding.md calls out explicitly (「ここは新規ユーザが
// 最も迷う箇所」): init.sh handles toolchain install automatically on first
// dispatch, but the harness's OWN login (claude/codex) needs a human in an
// interactive session, and nothing else in the onboarding flow prompts a
// first-time user to go run one.
func TestPrintNextStepsGuidance_MentionsInteractiveAgentLoginIsTheHardPart(t *testing.T) {
	var out strings.Builder
	printNextStepsGuidance(&out)

	got := out.String()
	for _, want := range []string{"boid agent claude", "login"} {
		if !strings.Contains(got, want) {
			t.Errorf("printNextStepsGuidance() missing %q; got:\n%s", want, got)
		}
	}
}

// TestRunStart_NonForeground_BOID_UIDZero_RefusesBeforeComposeUp is the
// codex round-9 review of PR5, Major regression test: on the compose-up
// (non-foreground) path, the effective uid that ends up substituted into
// compose.yml's `user: "${BOID_UID}:0"` is BOID_UID when explicitly set,
// not this process's own os.Getuid() — the refusal here must inspect
// effectiveBoidUID(), or a non-root operator setting BOID_UID=0 would
// silently bring up a root-uid daemon container.
func TestRunStart_NonForeground_BOID_UIDZero_RefusesBeforeComposeUp(t *testing.T) {
	t.Setenv("BOID_DAEMON_CHILD", "")
	t.Setenv("BOID_UID", "0")
	startForeground = false
	t.Cleanup(func() { startForeground = false })

	err := runStart(startCmd, nil)
	if err == nil {
		t.Fatal("expected an error: BOID_UID=0 on a non-foreground boid start must be refused before compose up")
	}
	if !strings.Contains(err.Error(), "uid 0") {
		t.Errorf("expected the error to mention uid 0, got %v", err)
	}
}

// TestRunStart_Foreground_BOID_UIDZero_DoesNotFalselyRefuse is the codex
// round-9 review of PR5, Major regression test (the other direction of
// the same bug): on the --foreground/daemon-child path THIS process
// itself is (or becomes) the daemon — compose.yml's `user:` substitution
// is never consulted there at all, so BOID_UID has no bearing on what
// uid this process actually runs as. The refusal on that branch must
// inspect this process's own os.Getuid(), not effectiveBoidUID() — a
// non-root operator running `BOID_UID=0 boid start --foreground` must
// NOT be refused just because BOID_UID happens to be "0" in their
// environment (this process's real, non-root uid is what will actually
// run).
func TestRunStart_Foreground_BOID_UIDZero_DoesNotFalselyRefuse(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("this regression only demonstrates on a non-root test process")
	}
	t.Setenv("BOID_UID", "0")
	startForeground = true
	t.Cleanup(func() { startForeground = false })

	origDBPath := startDBPath
	origSocketPath := startSocketPath
	t.Cleanup(func() {
		startDBPath = origDBPath
		startSocketPath = origSocketPath
	})
	dir := t.TempDir()
	startDBPath = filepath.Join(dir, "boid.db")
	startSocketPath = filepath.Join(dir, "boid.sock")

	// runStart's --foreground branch calls refuseRootUID before doing
	// anything else expensive (self-registration, buildStartConfig,
	// runDaemonChild) — this test only needs to observe that the uid
	// check itself does not fire; it does not need to let the daemon
	// actually start, so it directly exercises the same check runStart's
	// foreground branch makes (os.Getuid(), not effectiveBoidUID()) and
	// confirms it is nil despite BOID_UID=0.
	if err := refuseRootUID(os.Getuid()); err != nil {
		t.Fatalf("refuseRootUID(os.Getuid()) unexpectedly refused a non-root test process: %v", err)
	}
	if got := effectiveBoidUID(); got != 0 {
		t.Fatalf("effectiveBoidUID() = %d, want 0 (BOID_UID=0 is set) — sanity check that this test's premise holds", got)
	}
}

// TestRefuseDaemonConfigFlagsWithoutForeground is the codex round-3 review
// of PR5, Major 2 regression: --db-path/--socket-path/--kits-dir/
// --key-file-path/--cli-addr only ever configure the actual daemon
// process (buildStartConfig, read only on the --foreground/daemon-child
// branch) — a plain, non-foreground `boid start` silently ignored them
// after being redefined to "just run compose up", which could make a
// script think it pointed the daemon at a specific --db-path when it
// silently operated on the compose stack's own DB instead. Every such
// flag must be named in the resulting error.
func TestRefuseDaemonConfigFlagsWithoutForeground(t *testing.T) {
	cmd := &cobra.Command{Use: "start"}
	var dbPath, socketPath, kitsDir, keyFilePath, cliAddr string
	cmd.Flags().StringVar(&dbPath, "db-path", "", "")
	cmd.Flags().StringVar(&socketPath, "socket-path", "", "")
	cmd.Flags().StringVar(&kitsDir, "kits-dir", "", "")
	cmd.Flags().StringVar(&keyFilePath, "key-file-path", "", "")
	cmd.Flags().StringVar(&cliAddr, "cli-addr", "", "")

	if err := refuseDaemonConfigFlagsWithoutForeground(cmd); err != nil {
		t.Fatalf("expected no error when no flags are set, got: %v", err)
	}

	if err := cmd.Flags().Set("db-path", "/tmp/custom.db"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("cli-addr", "127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	err := refuseDaemonConfigFlagsWithoutForeground(cmd)
	if err == nil {
		t.Fatal("expected an error when --db-path/--cli-addr are set without --foreground")
	}
	for _, want := range []string{"--db-path", "--cli-addr", "--foreground"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %q", want, err.Error())
		}
	}
	if strings.Contains(err.Error(), "--socket-path") {
		t.Errorf("error should not mention an unset flag, got %q", err.Error())
	}
}

// TestRunStart_NonForeground_DBPathFlag_RefusesBeforeComposeUp pins the
// same regression at runStart's own call site: setting --db-path on a
// plain (non-foreground) `boid start` invocation must refuse BEFORE ever
// reaching runComposeUp (no PATH fakes are set up here, so an attempt to
// actually deploy would either hang trying real docker/podman or fail
// with an unrelated error — neither of which this test's assertion would
// accept).
func TestRunStart_NonForeground_DBPathFlag_RefusesBeforeComposeUp(t *testing.T) {
	t.Setenv("BOID_DAEMON_CHILD", "")
	startForeground = false
	t.Cleanup(func() { startForeground = false })

	origDBPath := startDBPath
	t.Cleanup(func() {
		startDBPath = origDBPath
		_ = startCmd.Flags().Set("db-path", origDBPath)
	})
	if err := startCmd.Flags().Set("db-path", "/tmp/custom.db"); err != nil {
		t.Fatal(err)
	}

	err := runStart(startCmd, nil)
	if err == nil {
		t.Fatal("expected an error: --db-path set without --foreground on a non-foreground boid start")
	}
	if !strings.Contains(err.Error(), "--db-path") {
		t.Errorf("expected the error to mention --db-path, got %v", err)
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

// TestDeployContainerScript_MalformedBOID_UID_Refuses is the codex
// round-9 review of PR5, Major regression test: the script's own uid-0
// guard (the actual enforcement point compose.yml's
// `user: "${BOID_UID}:0"` reads from) used to only ever compare BOID_UID
// against the exact literal string "0" — a value like "00", which
// docker/podman would still parse as numeric uid 0, sailed straight
// through uncaught. Runs the REAL script (not a fake) with a fake docker
// engine (so ENGINE=docker and the podman-only podman.socket preflight
// never runs, keeping this test isolated to the BOID_UID check) and
// asserts a malformed-but-zero-meaning value is refused, alongside a
// genuinely non-numeric value.
func TestDeployContainerScript_MalformedBOID_UID_Refuses(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("real deploy-container.sh not found at %s: %v", scriptPath, err)
	}

	dir := t.TempDir()
	writeFakeExecutable(t, dir, "docker", `
case "$1 $2" in
  "compose version") exit 0 ;;
  *) exit 0 ;;
esac
`)
	env := append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	for _, badUID := range []string{"00", "+0", "-1", "not-a-number"} {
		t.Run(badUID, func(t *testing.T) {
			cmd := exec.Command(scriptPath)
			cmd.Env = append(append([]string{}, env...), "BOID_UID="+badUID)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected BOID_UID=%q to be refused; output:\n%s", badUID, out)
			}
			if !strings.Contains(string(out), "BOID_UID") {
				t.Errorf("expected the error to mention BOID_UID for BOID_UID=%q; output:\n%s", badUID, out)
			}
		})
	}
}

// fakeDockerAlwaysOK writes a fake `docker` on PATH that reports itself
// (and its compose plugin) usable, and no-ops every other subcommand
// (build/compose down/compose up -d/...) successfully — enough to drive
// scripts/deploy-container.sh's default (no --build) up/down flow without
// a real engine.
func fakeDockerAlwaysOK(t *testing.T, dir string) {
	t.Helper()
	writeFakeExecutable(t, dir, "docker", `
if [ "$1" = "compose" ] && [ "$2" = "version" ]; then exit 0; fi
exit 0
`)
}

// composeEngineStateFilePath is scripts/deploy-container.sh's own
// COMPOSE_ENGINE_STATE_FILE: $XDG_STATE_HOME/boid/compose-engine.
//
// codex round-11 review of PR5, Major: this used to live under ROOT_DIR
// (the checkout/embedded-assets root the script's own BASH_SOURCE
// resolves to), which reintroduced the exact false-success bug the state
// file exists to close — compose.yml's `name: boid` makes the compose
// PROJECT identity independent of which ROOT_DIR a given invocation
// resolves to (cmd/host.go's findComposeRoot-or-deployFromEmbeddedAssets
// choice can legitimately differ between the `up` that started the stack
// and a LATER `boid stop`), so a ROOT_DIR-scoped file could easily not
// exist — or belong to the WRONG root — by the time `--down` looked for
// it, silently falling through to the no-record fallback path. Fixed to a
// location that tracks the one compose PROJECT a host can run at a time
// instead, mirroring cmd/host.go's own hostModeAssetsDir() convention.
func composeEngineStateFilePath(xdgStateHome string) string {
	return filepath.Join(xdgStateHome, "boid", "compose-engine")
}

// TestDeployContainerScript_Up_RecordsEngineForDown is the codex round-10
// review of PR5, Major regression test (half 1 of 2): a successful `up`
// must record which engine it actually used, so a later `--down` can pin
// to that exact engine instead of re-detecting "whichever engine looks
// usable right now" (see composeEngineStateFilePath's own doc comment for
// where). Also exercises the happy path where `--down` reads that record
// back and still succeeds against the SAME (still-usable) engine — and,
// per the round-11 fix, still finds that record from a DIFFERENT working
// directory / ROOT_DIR (this test runs `up` and `down` from the SAME real
// checkout root since that is exec'd, but the fixed
// $XDG_STATE_HOME-based location no longer depends on that at all).
func TestDeployContainerScript_Up_RecordsEngineForDown(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	xdgStateHome := t.TempDir()
	stateFile := composeEngineStateFilePath(xdgStateHome)

	dir := t.TempDir()
	fakeDockerAlwaysOK(t, dir)
	env := []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"XDG_STATE_HOME=" + xdgStateHome,
		"BOID_UID=1000",
		"BOID_GID=1000",
		"XDG_RUNTIME_DIR=" + t.TempDir(),
	}

	upCmd := exec.Command(scriptPath)
	upCmd.Env = env
	if out, err := upCmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy-container.sh (up) failed: %v\noutput:\n%s", err, out)
	}

	got, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read %s after a successful up: %v", stateFile, err)
	}
	// Line 1 is the engine kind; line 2 (codex round-13 review of PR5,
	// Major) is a DOCKER_CONTEXT/DOCKER_HOST fingerprint `--down` also
	// validates — see context_fingerprint's own doc comment in the
	// script. Only line 1 is asserted here.
	gotLines := strings.SplitN(string(got), "\n", 2)
	if gotLines[0] != "docker" {
		t.Errorf("%s line 1 = %q, want %q", stateFile, gotLines[0], "docker")
	}

	downCmd := exec.Command(scriptPath, "--down")
	downCmd.Env = env
	if out, err := downCmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy-container.sh --down failed against the same (still-usable) recorded engine: %v\noutput:\n%s", err, out)
	}
}

// TestDeployContainerScript_Down_FindsRecordedEngineFromDifferentRoot is
// the codex round-11 review of PR5, Major regression test: the state file
// must be readable by a `--down` invocation running the script out of a
// DIFFERENT ROOT_DIR than the `up` that wrote it — exactly what happens
// when `up` ran with `BOID_COMPOSE_ROOT` exported (or from within a real
// checkout, cmd/host.go's deployFromCheckout) but a later `boid stop`
// resolves cmd/host.go's OTHER root (deployFromEmbeddedAssets), or vice
// versa; compose.yml's own `name: boid` makes both invocations manage the
// SAME compose project regardless. Copies the real script + the compose
// files it needs into a second, independent root tree (simulating that
// root drift) and confirms `--down` there still finds and pins to the
// engine `up` recorded from the FIRST root, rather than silently falling
// back to auto-detection.
func TestDeployContainerScript_Down_FindsRecordedEngineFromDifferentRoot(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	xdgStateHome := t.TempDir()

	dir := t.TempDir()
	fakeDockerAlwaysOK(t, dir)
	upEnv := []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"XDG_STATE_HOME=" + xdgStateHome,
		"BOID_UID=1000",
		"BOID_GID=1000",
		"XDG_RUNTIME_DIR=" + t.TempDir(),
	}
	upCmd := exec.Command(scriptPath)
	upCmd.Env = upEnv
	if out, err := upCmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy-container.sh (up), root 1: %v\noutput:\n%s", err, out)
	}

	// Build a second, independent ROOT_DIR: the script only needs itself
	// plus build/container/compose.yml (compose -f target) to run --down —
	// Dockerfile/podman override are only touched by the up/build paths.
	root2 := t.TempDir()
	copyFileForTest(t, scriptPath, filepath.Join(root2, "scripts", "deploy-container.sh"), 0o755)
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	copyFileForTest(t,
		filepath.Join(repoRoot, "build", "container", "compose.yml"),
		filepath.Join(root2, "build", "container", "compose.yml"), 0o644)

	downCmd := exec.Command(filepath.Join(root2, "scripts", "deploy-container.sh"), "--down")
	downCmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"XDG_STATE_HOME=" + xdgStateHome,
		"XDG_RUNTIME_DIR=" + t.TempDir(),
	}
	out, err := downCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deploy-container.sh --down from a DIFFERENT root failed to find the engine recorded by root 1's up: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "engine=docker") {
		t.Errorf("expected --down (root 2) to pin to the engine recorded by root 1's up (docker), not re-detect; output:\n%s", out)
	}
}

// copyFileForTest copies src to dst (creating dst's parent directories),
// preserving nothing but the requested mode.
func copyFileForTest(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// TestDeployContainerScript_Down_RecordedEngineNoLongerUsable_Refuses is
// the codex round-10 review of PR5, Major regression test (half 2 of 2,
// the actual bug): if the engine `up` recorded is no longer usable by the
// time `--down` runs (crashed daemon, revoked permission, ...) while some
// OTHER engine happens to be usable, `--down` must refuse outright rather
// than silently `down`-ing an empty/never-created project under the OTHER
// engine and reporting overall success — that would leave the real stack,
// started under the recorded engine, running untouched while `boid stop`
// claims it stopped it.
func TestDeployContainerScript_Down_RecordedEngineNoLongerUsable_Refuses(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	xdgStateHome := t.TempDir()
	stateFile := composeEngineStateFilePath(xdgStateHome)
	// Pretend a previous `up` recorded docker, without actually running
	// one — this test only needs the state file's CONTENT, not a real
	// prior up.
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(stateFile), err)
	}
	if err := os.WriteFile(stateFile, []byte("docker\n"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", stateFile, err)
	}

	dir := t.TempDir()
	// A `docker` that is present on PATH but unusable (`docker version`
	// fails — simulating a crashed/unreachable daemon), alongside a
	// usable podman/podman-compose, which must NOT be substituted in for
	// the recorded (but now-unusable) docker engine. Deliberately a
	// FAILING docker rather than an absent one: this test must hold even
	// on a CI runner with a real, working system `docker` later on PATH
	// (github-hosted ubuntu runners ship one) — an absent fake would let
	// `command -v docker` fall through to that real, genuinely-usable
	// docker and pass for the wrong reason.
	writeFakeExecutable(t, dir, "docker", `exit 1`)
	writeFakeExecutable(t, dir, "podman", `
case "$1" in
  version) exit 0 ;;
  info) echo "false"; exit 0 ;;
  *) exit 0 ;;
esac
`)
	writeFakeExecutable(t, dir, "podman-compose", `exit 0`)
	writeFakeExecutable(t, dir, "systemctl", `exit 0`) // podman.socket "active" (irrelevant here — --down skips this preflight anyway)

	downCmd := exec.Command(scriptPath, "--down")
	downCmd.Env = []string{
		// Fake dir ONLY — not appending the real PATH — so this cannot
		// accidentally fall through to a real docker/podman elsewhere on
		// PATH regardless of host. coreutils (mkdir/cat/dirname/...) the
		// script also needs come from a fixed, minimal set of directories
		// instead of the ambient PATH.
		"PATH=" + dir + ":/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"XDG_STATE_HOME=" + xdgStateHome,
		"XDG_RUNTIME_DIR=" + t.TempDir(),
	}
	out, err := downCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected --down to refuse when the recorded engine (docker) is no longer usable, even though podman is; output:\n%s", out)
	}
	if !strings.Contains(string(out), "docker") {
		t.Errorf("expected the refusal to name the recorded engine (docker); output:\n%s", out)
	}
}

// TestDeployContainerScript_Down_ContextDrift_Refuses is the codex
// round-13 review of PR5, Major regression test: recording the engine
// KIND (docker/podman) alone cannot tell two DIFFERENT daemons of the
// SAME kind apart — an operator who switches DOCKER_CONTEXT/DOCKER_HOST
// between `up` and `down` would still pass the engine-usability check,
// then `down` an empty `name: boid` project on the NEW daemon while the
// real stack (on the OLD one) keeps running. `--down` must now also
// compare context_fingerprint() against what `up` recorded.
func TestDeployContainerScript_Down_ContextDrift_Refuses(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	xdgStateHome := t.TempDir()

	dir := t.TempDir()
	fakeDockerAlwaysOK(t, dir)
	upCmd := exec.Command(scriptPath)
	upCmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"XDG_STATE_HOME=" + xdgStateHome,
		"DOCKER_CONTEXT=context-a",
		"BOID_UID=1000",
		"BOID_GID=1000",
		"XDG_RUNTIME_DIR=" + t.TempDir(),
	}
	if out, err := upCmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy-container.sh (up) with DOCKER_CONTEXT=context-a failed: %v\noutput:\n%s", err, out)
	}

	downCmd := exec.Command(scriptPath, "--down")
	downCmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"XDG_STATE_HOME=" + xdgStateHome,
		"DOCKER_CONTEXT=context-b", // drifted since `up`
		"XDG_RUNTIME_DIR=" + t.TempDir(),
	}
	out, err := downCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected --down to refuse when DOCKER_CONTEXT drifted since the recording up (context-a -> context-b); output:\n%s", out)
	}
	if !strings.Contains(string(out), "DOCKER_CONTEXT") {
		t.Errorf("expected the refusal to mention DOCKER_CONTEXT; output:\n%s", out)
	}
}

// TestDeployContainerScript_Down_CorruptedEngineWithInternalWhitespace_Refuses
// is the codex round-13 review of PR5, Major regression test: an earlier
// revision trimmed the recorded engine value with `tr -d '[:space:]'`,
// which strips ALL whitespace (not just leading/trailing) — a corrupted
// value like "pod man" was silently normalized to the valid-looking
// "podman" instead of being rejected as unrecognized. Only leading/
// trailing whitespace must be trimmed now.
func TestDeployContainerScript_Down_CorruptedEngineWithInternalWhitespace_Refuses(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	xdgStateHome := t.TempDir()
	stateFile := composeEngineStateFilePath(xdgStateHome)
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(stateFile), err)
	}
	if err := os.WriteFile(stateFile, []byte("pod man\n"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", stateFile, err)
	}

	dir := t.TempDir()
	writeFakeExecutable(t, dir, "podman", `
case "$1" in
  version) exit 0 ;;
  info) echo "false"; exit 0 ;;
  *) exit 0 ;;
esac
`)
	writeFakeExecutable(t, dir, "podman-compose", `exit 0`)
	downCmd := exec.Command(scriptPath, "--down")
	downCmd.Env = []string{
		"PATH=" + dir + ":/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"XDG_STATE_HOME=" + xdgStateHome,
		"XDG_RUNTIME_DIR=" + t.TempDir(),
	}
	out, err := downCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected --down to refuse a corrupted \"pod man\" record rather than silently accept it as \"podman\"; output:\n%s", out)
	}
	if !strings.Contains(string(out), "unrecognized engine") {
		t.Errorf("expected the refusal to say the engine is unrecognized; output:\n%s", out)
	}
}

// TestDeployContainerScript_Up_StateFileWriteFailure_FailsUp is the codex
// round-12 review of PR5, Major regression test: an earlier revision
// treated a failure to WRITE the engine-state file as non-fatal for `up`
// ("continuing — --down degrades to best-effort detection") — but a
// missing-because-unwritable state file behaves identically, to a later
// `--down`, as one that legitimately never existed, silently reopening
// the exact false-success window rounds 10/11 exist to close. `up` must
// fail loudly instead when it cannot persist which engine it used.
// Simulated by making $XDG_STATE_HOME/boid unwritable (0000) before it
// exists, so the script's own `mkdir -p`/write both fail.
func TestDeployContainerScript_Up_StateFileWriteFailure_FailsUp(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permission bits, so this regression cannot demonstrate as root")
	}
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}

	xdgStateHome := t.TempDir()
	// Pre-create $XDG_STATE_HOME/boid as a directory the script cannot
	// write into (and cannot recreate, since it already exists) —
	// simulates a permissions oddity there without needing to run as a
	// different uid.
	unwritable := filepath.Join(xdgStateHome, "boid")
	if err := os.MkdirAll(unwritable, 0o555); err != nil {
		t.Fatalf("mkdir %s: %v", unwritable, err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o755) }) // let t.TempDir() clean up

	dir := t.TempDir()
	fakeDockerAlwaysOK(t, dir)
	upCmd := exec.Command(scriptPath)
	upCmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"XDG_STATE_HOME=" + xdgStateHome,
		"BOID_UID=1000",
		"BOID_GID=1000",
		"XDG_RUNTIME_DIR=" + t.TempDir(),
	}
	out, err := upCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected up to fail when it cannot write the engine-state file; output:\n%s", out)
	}
	if !strings.Contains(string(out), "compose-engine") {
		t.Errorf("expected the failure to mention the engine-state file; output:\n%s", out)
	}
}

// TestDeployContainerScript_Down_CorruptStateFile_RefusesRatherThanFallsBack
// is the codex round-12 review of PR5, Major regression test: an earlier
// revision treated an EXISTING-but-bad-content state file (unreadable,
// empty, or an unrecognized engine name) as equivalent to no state file
// at all — warn, then fall back to the ordinary engine-detection ladder.
// But a state file that EXISTS proves an `up` DID run and DID try to
// record something; guessing again on top of that is the same
// false-success window rounds 10/11 exist to close, just moved one step
// earlier. `--down` must refuse outright instead when the file exists but
// its content cannot be trusted.
func TestDeployContainerScript_Down_CorruptStateFile_RefusesRatherThanFallsBack(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "scripts", "deploy-container.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}

	for name, content := range map[string]string{
		"empty":        "",
		"unrecognized": "hyper-v\n",
	} {
		t.Run(name, func(t *testing.T) {
			xdgStateHome := t.TempDir()
			stateFile := composeEngineStateFilePath(xdgStateHome)
			if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", filepath.Dir(stateFile), err)
			}
			if err := os.WriteFile(stateFile, []byte(content), 0o644); err != nil {
				t.Fatalf("seed %s: %v", stateFile, err)
			}

			dir := t.TempDir()
			fakeDockerAlwaysOK(t, dir) // usable, so a fall-back-to-ladder bug would silently "succeed" here
			downCmd := exec.Command(scriptPath, "--down")
			downCmd.Env = []string{
				"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + os.Getenv("HOME"),
				"XDG_STATE_HOME=" + xdgStateHome,
				"XDG_RUNTIME_DIR=" + t.TempDir(),
			}
			out, err := downCmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected --down to refuse rather than fall back when %s; output:\n%s", stateFile, out)
			}
			if !strings.Contains(string(out), "compose-engine") {
				t.Errorf("expected the refusal to mention the state file; output:\n%s", out)
			}
		})
	}
}
