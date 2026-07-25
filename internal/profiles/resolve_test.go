package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
)

// cmdWithProfileFlag builds a minimal *cobra.Command carrying the same
// --profile flag cmd/root.go registers on rootCmd, optionally pre-set to
// value (empty means "not passed").
func cmdWithProfileFlag(t *testing.T, value string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String(ProfileFlagName, "", "")
	if value != "" {
		if err := cmd.Flags().Set(ProfileFlagName, value); err != nil {
			t.Fatalf("set --profile: %v", err)
		}
	}
	return cmd
}

func writeResolveConfig(t *testing.T, content string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if err := os.MkdirAll(filepath.Join(configDir, "boid"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "boid", "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// noSocketAt isolates DefaultSocketPath() to a path that provably does not
// exist, so the terminal-fallback auto-detect (SourceUnixFallback vs
// SourceTCPFallback) deterministically picks the TCP branch regardless of
// what the host running this test actually has listening — a real
// XDG_RUNTIME_DIR/boid.sock (or /run/user/<uid>/boid.sock) could otherwise
// leak in from the ambient environment and make this test flaky depending
// on what else is running on the machine.
func noSocketAt(t *testing.T) {
	t.Helper()
	t.Setenv("BOID_SOCKET", filepath.Join(t.TempDir(), "does-not-exist.sock"))
}

// TestResolve_NoConfigNoProfile_FallsBackToTCP pins the docs/plans/
// volume-only-daemon.md §論点c terminal-fallback behavior, TCP half: with
// no --profile/BOID_PROFILE/default_profile at all AND no unix socket file
// reachable at client.DefaultSocketPath(), Resolve falls back to
// client.DefaultCLIAddr() over TCP+TLS — the compose/named-volume
// deployment case (PR-3 codex round-1 design pivot: this is now one of
// TWO auto-detected fallback branches, not an unconditional cutover — see
// TestResolve_NoConfigNoProfile_UnixSocketExists_FallsBackToUnix for the
// other one).
func TestResolve_NoConfigNoProfile_FallsBackToTCP(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no config.yaml written
	t.Setenv("BOID_CLI_ADDR", "127.0.0.1:19999")
	noSocketAt(t)

	rp, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Source != SourceTCPFallback {
		t.Errorf("Source = %q, want %q", rp.Source, SourceTCPFallback)
	}
	want := "https://" + client.DefaultCLIAddr()
	if rp.URL != want {
		t.Errorf("URL = %q, want %q", rp.URL, want)
	}
	if rp.Name != "" {
		t.Errorf("Name = %q, want empty", rp.Name)
	}
	if rp.Token != "" {
		t.Errorf("Token = %q, want empty (TCP fallback never needs one — loopback trust)", rp.Token)
	}
}

// TestResolve_NoConfigNoProfile_UnixSocketExists_FallsBackToUnix pins the
// OTHER auto-detected terminal-fallback branch (PR-3 codex round-1 design
// pivot): when a file exists at client.DefaultSocketPath() — the userns
// backend / bare `boid start` / every Black-box E2E scenario's own shape —
// Resolve keeps dialing it directly, exactly like every pre-§論点c
// terminal fallback did, instead of the TCP branch.
func TestResolve_NoConfigNoProfile_UnixSocketExists_FallsBackToUnix(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no config.yaml written
	socketPath := filepath.Join(t.TempDir(), "boid.sock")
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatalf("seed fake socket file: %v", err)
	}
	t.Setenv("BOID_SOCKET", socketPath)

	rp, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Source != SourceUnixFallback {
		t.Errorf("Source = %q, want %q", rp.Source, SourceUnixFallback)
	}
	if !rp.IsUnix() {
		t.Errorf("IsUnix() = false, want true")
	}
	want := "unix://" + socketPath
	if rp.URL != want {
		t.Errorf("URL = %q, want %q", rp.URL, want)
	}
	if rp.Token != "" {
		t.Errorf("Token = %q, want empty (unix never needs one)", rp.Token)
	}
}

// TestResolve_NoConfigNoProfile_TCPFallback_LoadsCACertIfPresent pins the
// bootstrap-file half of the TCP fallback: when
// client.DefaultCACertPath() exists (written by a bare `boid start` on
// every boot, or by deploy-container.sh from `boid start
// --print-cli-profile`'s output), Resolve loads its contents into CACert so
// client.NewClientWithCACert can trust the daemon's self-signed internal CA.
func TestResolve_NoConfigNoProfile_TCPFallback_LoadsCACertIfPresent(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir) // no config.yaml written
	if err := os.MkdirAll(filepath.Join(configDir, "boid"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "boid", "daemon-ca.pem"), []byte("fake-ca-pem"), 0o600); err != nil {
		t.Fatalf("write daemon-ca.pem: %v", err)
	}

	rp, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.CACert != "fake-ca-pem" {
		t.Errorf("CACert = %q, want %q", rp.CACert, "fake-ca-pem")
	}
}

// TestResolve_NoConfigNoProfile_TCPFallback_NoCACertFile_EmptyCACert pins
// the absent-file case: no daemon-ca.pem means CACert stays empty (not an
// error) — the eventual dial just falls back to the system cert pool, which
// then fails TLS verification against the daemon's self-signed cert with an
// ordinary error at request time.
func TestResolve_NoConfigNoProfile_TCPFallback_NoCACertFile_EmptyCACert(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no config.yaml, no daemon-ca.pem

	rp, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.CACert != "" {
		t.Errorf("CACert = %q, want empty", rp.CACert)
	}
}

func TestResolve_FlagTakesPrecedenceOverEnvAndDefault(t *testing.T) {
	writeResolveConfig(t, `
default_profile: by-default
profiles:
  by-default:
    url: unix:///tmp/by-default.sock
  by-env:
    url: unix:///tmp/by-env.sock
  by-flag:
    url: unix:///tmp/by-flag.sock
`)
	t.Setenv(BOIDProfileEnv, "by-env")
	cmd := cmdWithProfileFlag(t, "by-flag")

	rp, err := Resolve(cmd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Name != "by-flag" || rp.Source != SourceFlag {
		t.Errorf("got Name=%q Source=%q, want by-flag/flag", rp.Name, rp.Source)
	}
}

func TestResolve_EnvTakesPrecedenceOverDefault(t *testing.T) {
	writeResolveConfig(t, `
default_profile: by-default
profiles:
  by-default:
    url: unix:///tmp/by-default.sock
  by-env:
    url: unix:///tmp/by-env.sock
`)
	t.Setenv(BOIDProfileEnv, "by-env")

	rp, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Name != "by-env" || rp.Source != SourceEnv {
		t.Errorf("got Name=%q Source=%q, want by-env/env", rp.Name, rp.Source)
	}
}

func TestResolve_DefaultProfileUsedWhenNoFlagOrEnv(t *testing.T) {
	writeResolveConfig(t, `
default_profile: by-default
profiles:
  by-default:
    url: unix:///tmp/by-default.sock
`)

	rp, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Name != "by-default" || rp.Source != SourceDefaultProfile {
		t.Errorf("got Name=%q Source=%q, want by-default/default_profile", rp.Name, rp.Source)
	}
	if rp.URL != "unix:///tmp/by-default.sock" {
		t.Errorf("URL = %q", rp.URL)
	}
}

func TestResolve_UnknownProfile_Error(t *testing.T) {
	writeResolveConfig(t, `
profiles:
  home:
    url: unix:///tmp/home.sock
`)
	cmd := cmdWithProfileFlag(t, "ghost")

	_, err := Resolve(cmd)
	if err == nil {
		t.Fatal("expected an error for an undefined profile name")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not defined") {
		t.Errorf("error should name the profile and say it's undefined, got %q", err.Error())
	}
}

func TestResolve_EmptyFlag_HardError(t *testing.T) {
	// An explicit `--profile=` (Changed=true, value="") is a caller
	// mistake, not an implicit request for the unix fallback. Falling
	// back would ALSO skip slug validation, so hard-fail up front.
	writeResolveConfig(t, `profiles: {}`)

	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String(ProfileFlagName, "", "")
	// Set to empty explicitly — Set() marks Changed=true even for "".
	if err := cmd.Flags().Set(ProfileFlagName, ""); err != nil {
		t.Fatalf("set: %v", err)
	}

	_, err := Resolve(cmd)
	if err == nil {
		t.Fatal("expected a hard error for --profile=\"\"")
	}
	if !strings.Contains(err.Error(), "--profile requires a non-empty value") {
		t.Errorf("error should mention that --profile needs a value, got %q", err.Error())
	}
}

func TestResolve_UnsupportedScheme_HardError_BeforeTokenLookup(t *testing.T) {
	// A profile with an unsupported scheme must fail with an
	// unsupported-scheme error — NOT with the "run 'boid login'" message
	// LoadToken's not-exist branch would surface if we reached that step.
	writeResolveConfig(t, `
profiles:
  bogus:
    url: ftp://example.com
`)
	cmd := cmdWithProfileFlag(t, "bogus")

	_, err := Resolve(cmd)
	if err == nil {
		t.Fatal("expected an error for an unsupported url scheme")
	}
	if !strings.Contains(err.Error(), "unsupported url scheme") {
		t.Errorf("error should mention unsupported scheme, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "boid login") {
		t.Errorf("error should NOT direct the user to `boid login` (the scheme is the problem, not a missing token): %q", err.Error())
	}
}

func TestResolve_InvalidSlug_Error(t *testing.T) {
	writeResolveConfig(t, `profiles: {}`)
	t.Setenv(BOIDProfileEnv, "../etc/passwd")

	_, err := Resolve(nil)
	if err == nil {
		t.Fatal("expected an error for a path-traversal-shaped profile name")
	}
}

func TestResolve_UnixProfile_NoTokenRequired(t *testing.T) {
	writeResolveConfig(t, `
profiles:
  home:
    url: unix:///tmp/home.sock
`)
	cmd := cmdWithProfileFlag(t, "home")

	rp, err := Resolve(cmd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Token != "" {
		t.Errorf("Token = %q, want empty for a unix-scheme profile", rp.Token)
	}
}

func TestResolve_HTTPSProfile_MissingToken_Error(t *testing.T) {
	writeResolveConfig(t, `
profiles:
  work:
    url: https://work.example.com
`)
	cmd := cmdWithProfileFlag(t, "work")

	_, err := Resolve(cmd)
	if err == nil {
		t.Fatal("expected an error when no token file exists for an https profile")
	}
	if !strings.Contains(err.Error(), "no device token") || !strings.Contains(err.Error(), "boid login") {
		t.Errorf("error should match the spec's message shape, got %q", err.Error())
	}
}

func TestResolve_HTTPSProfile_TokenURLMismatch_Error(t *testing.T) {
	writeResolveConfig(t, `
profiles:
  work:
    url: https://work.example.com
`)
	writeTokenFile(t, "work", `{"device_id":"d","token":"t","url":"https://old.example.com"}`, 0o600)
	cmd := cmdWithProfileFlag(t, "work")

	_, err := Resolve(cmd)
	if err == nil {
		t.Fatal("expected a hard error for a config/token URL mismatch")
	}
	if !strings.Contains(err.Error(), "URL mismatch") ||
		!strings.Contains(err.Error(), "config=https://work.example.com") ||
		!strings.Contains(err.Error(), "token=https://old.example.com") {
		t.Errorf("error should match the spec's message shape, got %q", err.Error())
	}
}

// TestResolve_NamedLoopbackProfile_NoTokenRequired pins the PR-3 codex
// round-1 Blocker fix: a NAMED https profile pointed at a loopback host
// (e.g. deploy-container.sh's compose-seeded default_profile,
// "https://127.0.0.1:8442") must resolve successfully with NO token file
// on disk at all — unlike TestResolve_HTTPSProfile_MissingToken_Error
// (a genuinely remote host), which still hard-errors.
func TestResolve_NamedLoopbackProfile_NoTokenRequired(t *testing.T) {
	writeResolveConfig(t, `
profiles:
  default:
    url: https://127.0.0.1:8442
`)
	cmd := cmdWithProfileFlag(t, "default")

	rp, err := Resolve(cmd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Token != "" {
		t.Errorf("Token = %q, want empty (loopback trust — no token file exists)", rp.Token)
	}
}

// TestResolve_NamedLoopbackProfile_UsesTokenIfPresent pins the
// best-effort half: when a token file DOES exist and matches the profile's
// URL, it is still loaded and sent — loopback trust makes it optional, not
// forbidden.
func TestResolve_NamedLoopbackProfile_UsesTokenIfPresent(t *testing.T) {
	writeResolveConfig(t, `
profiles:
  default:
    url: https://127.0.0.1:8442
`)
	writeTokenFile(t, "default", `{"device_id":"d","token":"tk_loopback","url":"https://127.0.0.1:8442"}`, 0o600)
	cmd := cmdWithProfileFlag(t, "default")

	rp, err := Resolve(cmd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Token != "tk_loopback" {
		t.Errorf("Token = %q, want %q", rp.Token, "tk_loopback")
	}
}

// TestResolve_NamedLoopbackProfile_StaleTokenIgnoredNotHardError pins the
// "best-effort, not exclusive" half of the loopback exemption: a token
// file that exists but names a DIFFERENT URL (stale from an earlier
// profile edit) must not turn an otherwise-working, tokenless loopback
// request into a hard failure the way the non-loopback branch's URL-
// mismatch check does.
func TestResolve_NamedLoopbackProfile_StaleTokenIgnoredNotHardError(t *testing.T) {
	writeResolveConfig(t, `
profiles:
  default:
    url: https://127.0.0.1:8442
`)
	writeTokenFile(t, "default", `{"device_id":"d","token":"stale","url":"https://127.0.0.1:9999"}`, 0o600)
	cmd := cmdWithProfileFlag(t, "default")

	rp, err := Resolve(cmd)
	if err != nil {
		t.Fatalf("Resolve: %v, want no error (stale token must be ignored, not hard-fail)", err)
	}
	if rp.Token != "" {
		t.Errorf("Token = %q, want empty (mismatched token silently ignored)", rp.Token)
	}
}

func TestResolve_HTTPSProfile_ValidToken_Success(t *testing.T) {
	writeResolveConfig(t, `
profiles:
  work:
    url: https://work.example.com
`)
	writeTokenFile(t, "work", `{"device_id":"d","token":"tk_secret","url":"https://work.example.com"}`, 0o600)
	cmd := cmdWithProfileFlag(t, "work")

	rp, err := Resolve(cmd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Token != "tk_secret" {
		t.Errorf("Token = %q, want %q", rp.Token, "tk_secret")
	}
	if rp.URL != "https://work.example.com" {
		t.Errorf("URL = %q", rp.URL)
	}
}
