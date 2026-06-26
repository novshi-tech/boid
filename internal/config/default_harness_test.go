package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHarnessName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"claude", "claude", true},
		{"codex", "codex", true},
		{"opencode", "opencode", true},
		{"with-hyphen", "my-harness", true},
		{"with-underscore", "my_harness", true},
		{"with-digits", "claude2", true},
		{"empty", "", false},
		{"leading-digit", "2claude", false},
		{"leading-hyphen", "-claude", false},
		{"slash", "claude/codex", false},
		{"space", "my harness", false},
		{"dot", "claude.exe", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateHarnessName(c.in)
			if c.ok && err != nil {
				t.Fatalf("ValidateHarnessName(%q) unexpected error: %v", c.in, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("ValidateHarnessName(%q) should have errored", c.in)
			}
		})
	}
}

func TestDefaultHarness_EnvWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv(EnvDefaultHarness, "codex")

	// Write a config with a different harness; env should win.
	cfgDir := filepath.Join(dir, "boid")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cfgDir, "config.yaml"),
		[]byte("default_harness: claude\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	got, err := DefaultHarness()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "codex" {
		t.Errorf("DefaultHarness: got %q, want %q", got, "codex")
	}
}

func TestDefaultHarness_EnvInvalid(t *testing.T) {
	t.Setenv(EnvDefaultHarness, "bad name")
	_, err := DefaultHarness()
	if err == nil {
		t.Fatal("expected error for invalid env value")
	}
	if !strings.Contains(err.Error(), EnvDefaultHarness) {
		t.Errorf("error should mention env var: %v", err)
	}
}

func TestDefaultHarness_FileWhenEnvUnset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv(EnvDefaultHarness, "")

	cfgDir := filepath.Join(dir, "boid")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cfgDir, "config.yaml"),
		[]byte("default_harness: opencode\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	got, err := DefaultHarness()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "opencode" {
		t.Errorf("DefaultHarness: got %q, want %q", got, "opencode")
	}
}

func TestDefaultHarness_FileInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv(EnvDefaultHarness, "")

	cfgDir := filepath.Join(dir, "boid")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Write a syntactically-valid YAML with an invalid harness name.
	if err := os.WriteFile(
		filepath.Join(cfgDir, "config.yaml"),
		[]byte("default_harness: \"bad name\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	_, err := DefaultHarness()
	if err == nil {
		t.Fatal("expected error for invalid file value")
	}
}

func TestDefaultHarness_UnsetReturnsSentinel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv(EnvDefaultHarness, "")

	_, err := DefaultHarness()
	if !errors.Is(err, ErrDefaultHarnessNotSet) {
		t.Fatalf("got %v, want ErrDefaultHarnessNotSet", err)
	}
}

func TestSetDefaultHarnessAt_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boid", "config.yaml")

	if err := setDefaultHarnessAt(path, "claude"); err != nil {
		t.Fatalf("setDefaultHarnessAt: %v", err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("loadFromPath: %v", err)
	}
	if cfg.DefaultHarness != "claude" {
		t.Errorf("DefaultHarness: got %q, want %q", cfg.DefaultHarness, "claude")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm: got %v, want 0600", perm)
	}
}

func TestSetDefaultHarnessAt_PreservesExistingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	existing := "gc:\n  interval: 6h\nweb:\n  public_url: https://example.com\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := setDefaultHarnessAt(path, "codex"); err != nil {
		t.Fatalf("setDefaultHarnessAt: %v", err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("loadFromPath: %v", err)
	}
	if cfg.DefaultHarness != "codex" {
		t.Errorf("DefaultHarness: got %q, want %q", cfg.DefaultHarness, "codex")
	}
	if cfg.Web.PublicURL != "https://example.com" {
		t.Errorf("Web.PublicURL preserved: got %q", cfg.Web.PublicURL)
	}
	if cfg.GC.Interval.String() != "6h0m0s" {
		t.Errorf("GC.Interval preserved: got %v", cfg.GC.Interval)
	}
}

func TestSetDefaultHarnessAt_Overwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := setDefaultHarnessAt(path, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := setDefaultHarnessAt(path, "opencode"); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("loadFromPath: %v", err)
	}
	if cfg.DefaultHarness != "opencode" {
		t.Errorf("DefaultHarness: got %q, want %q", cfg.DefaultHarness, "opencode")
	}
}

func TestSetDefaultHarness_ValidatesInput(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SetDefaultHarness("bad name"); err == nil {
		t.Fatal("expected validation error")
	}
	if err := SetDefaultHarness(""); err == nil {
		t.Fatal("expected validation error for empty string")
	}
}

// Ensure no leftover temp file is left behind on a happy path.
func TestSetDefaultHarnessAt_NoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := setDefaultHarnessAt(path, "claude"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "config.yaml" {
			continue
		}
		t.Errorf("leftover file in dir: %s", e.Name())
	}
}
