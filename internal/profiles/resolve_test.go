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

func TestResolve_NoConfigNoProfile_FallsBackToUnix(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no config.yaml written
	t.Setenv("BOID_SOCKET", "/tmp/pinned-for-test.sock")

	rp, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rp.Source != SourceUnixFallback {
		t.Errorf("Source = %q, want %q", rp.Source, SourceUnixFallback)
	}
	want := "unix://" + client.DefaultSocketPath()
	if rp.URL != want {
		t.Errorf("URL = %q, want %q", rp.URL, want)
	}
	if rp.Name != "" {
		t.Errorf("Name = %q, want empty", rp.Name)
	}
	if rp.Token != "" {
		t.Errorf("Token = %q, want empty (unix fallback never needs one)", rp.Token)
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
