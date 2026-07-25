package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/profiles"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
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
	// docs/plans/volume-only-daemon.md §論点c: CLIAddr defaults to
	// client.DefaultCLIAddr() ("127.0.0.1:8442") when no --cli-addr
	// override is given, exactly like every other opts.* field above.
	if cfg.CLIAddr != "127.0.0.1:8442" {
		t.Fatalf("CLIAddr = %q, want %q", cfg.CLIAddr, "127.0.0.1:8442")
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

// --- boid start --print-cli-profile (docs/plans/volume-only-daemon.md
// §論点c "profile bootstrapping") ---

// TestPrintCLIProfile_PrintsValidProfileYAML pins the whole point of
// --print-cli-profile: its stdout output parses as a profiles.Config
// document naming a "default" profile at cfg.CLIAddr, with a non-empty
// ca_cert that round-trips as the SAME CA cfg.TLSDir holds (mtls.LoadOrCreate
// is idempotent — printCLIProfile must load the existing CA, not silently
// generate and discard a second one).
func TestPrintCLIProfile_PrintsValidProfileYAML(t *testing.T) {
	tmpDir := t.TempDir()
	tlsDir := filepath.Join(tmpDir, "tls")

	// Pre-create the CA (mirrors a daemon that already booted once — the
	// deploy-container.sh flow this flag is designed for, "run it after
	// daemon boot").
	ca, err := mtls.LoadOrCreate(tlsDir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	cfg, err := buildStartConfig(startConfigOptions{})
	if err != nil {
		t.Fatalf("buildStartConfig: %v", err)
	}
	// buildStartConfig always recomputes TLSDir from XDG_DATA_HOME
	// (defaultTLSDir) — point it at the pre-seeded tlsDir above rather than
	// re-deriving it, so this test controls exactly which CA gets loaded.
	cfg.TLSDir = tlsDir
	cfg.CLIAddr = "127.0.0.1:18442"

	cmd := &cobra.Command{Use: "start"}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := printCLIProfile(cmd, cfg); err != nil {
		t.Fatalf("printCLIProfile: %v", err)
	}

	var doc profiles.Config
	if err := yaml.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output did not parse as a profiles.Config document: %v\noutput:\n%s", err, out.String())
	}
	if doc.DefaultProfile != bootstrapProfileName {
		t.Errorf("default_profile = %q, want %q", doc.DefaultProfile, bootstrapProfileName)
	}
	prof, ok := doc.Profiles[bootstrapProfileName]
	if !ok {
		t.Fatalf("profiles.%s entry missing from output:\n%s", bootstrapProfileName, out.String())
	}
	if prof.URL != "https://127.0.0.1:18442" {
		t.Errorf("url = %q, want %q", prof.URL, "https://127.0.0.1:18442")
	}
	if prof.CACert == "" {
		t.Fatal("ca_cert is empty, want the daemon CA's own PEM cert")
	}
	if prof.CACert != string(ca.CertPEM()) {
		t.Errorf("ca_cert does not match the pre-seeded CA's own cert (printCLIProfile must LOAD the existing CA, not generate a fresh, different one)")
	}
}

// TestPrintCLIProfile_DoesNotStartDaemon pins the "does NOT start the
// daemon" half of the flag's contract: no socket/listener files exist
// after printCLIProfile returns — only the tls/ directory (from
// mtls.LoadOrCreate) is touched.
func TestPrintCLIProfile_DoesNotStartDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	cfg, err := buildStartConfig(startConfigOptions{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
	})
	if err != nil {
		t.Fatalf("buildStartConfig: %v", err)
	}

	cmd := &cobra.Command{Use: "start"}
	cmd.SetOut(&bytes.Buffer{})
	if err := printCLIProfile(cmd, cfg); err != nil {
		t.Fatalf("printCLIProfile: %v", err)
	}

	if _, err := os.Stat(cfg.SocketPath); !os.IsNotExist(err) {
		t.Errorf("socket file exists at %s after --print-cli-profile, want no daemon started", cfg.SocketPath)
	}
	if _, err := os.Stat(cfg.DBPath); !os.IsNotExist(err) {
		t.Errorf("db file exists at %s after --print-cli-profile, want no daemon started", cfg.DBPath)
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
