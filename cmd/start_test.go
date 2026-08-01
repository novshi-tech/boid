package cmd

import (
	"os"
	"path/filepath"
	"testing"
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
