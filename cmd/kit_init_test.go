package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/config"
)

// withIsolatedConfigHome routes XDG_CONFIG_HOME (and HOME, as a fallback for
// os.UserConfigDir) to a per-test tempdir and clears env vars that could leak
// into the default-harness resolver. It returns the config dir so tests can
// inspect what was written.
func withIsolatedConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv(config.EnvDefaultHarness, "")
	return dir
}

func TestRunKitInit_PromptsAndPersists(t *testing.T) {
	dir := withIsolatedConfigHome(t)

	in := strings.NewReader("claude\n")
	var out bytes.Buffer
	if err := runKitInit(in, &out); err != nil {
		t.Fatalf("runKitInit: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"No default harness configured",
		"saved default harness: claude",
		"default harness: claude",
		"生成スキルは今後の PR で実装予定",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n---\n%s", want, got)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.DefaultHarness != "claude" {
		t.Errorf("persisted DefaultHarness: got %q, want %q", cfg.DefaultHarness, "claude")
	}

	// Sanity: the file landed where we expect.
	cfgPath := filepath.Join(dir, "boid", "config.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config file should exist at %s: %v", cfgPath, err)
	}
}

func TestRunKitInit_ReusesExistingValue(t *testing.T) {
	withIsolatedConfigHome(t)
	if err := config.SetDefaultHarness("opencode"); err != nil {
		t.Fatalf("SetDefaultHarness: %v", err)
	}

	in := strings.NewReader("") // should never be read
	var out bytes.Buffer
	if err := runKitInit(in, &out); err != nil {
		t.Fatalf("runKitInit: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "No default harness configured") {
		t.Errorf("should not prompt when harness already set; output:\n%s", got)
	}
	if !strings.Contains(got, "default harness: opencode") {
		t.Errorf("output missing harness line:\n%s", got)
	}
}

func TestRunKitInit_RetriesOnInvalidInput(t *testing.T) {
	withIsolatedConfigHome(t)

	in := strings.NewReader("bad name\n2bad\nclaude\n")
	var out bytes.Buffer
	if err := runKitInit(in, &out); err != nil {
		t.Fatalf("runKitInit: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.DefaultHarness != "claude" {
		t.Errorf("got %q, want %q", cfg.DefaultHarness, "claude")
	}

	// Each invalid attempt should have surfaced the validation error.
	got := out.String()
	if strings.Count(got, "invalid harness name") < 2 {
		t.Errorf("expected at least 2 validation error lines; got:\n%s", got)
	}
}

func TestRunKitInit_GivesUpAfterMaxAttempts(t *testing.T) {
	withIsolatedConfigHome(t)

	in := strings.NewReader("bad name\nworse!\n2bad\n")
	var out bytes.Buffer
	err := runKitInit(in, &out)
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if !strings.Contains(err.Error(), "default harness not provided") {
		t.Errorf("error message: %v", err)
	}
}

func TestRunKitInit_EmptyStdinFailsCleanly(t *testing.T) {
	withIsolatedConfigHome(t)

	in := strings.NewReader("")
	var out bytes.Buffer
	err := runKitInit(in, &out)
	if err == nil {
		t.Fatal("expected error when stdin is empty")
	}
	if !strings.Contains(err.Error(), config.EnvDefaultHarness) {
		t.Errorf("error should mention env var fallback: %v", err)
	}
}

func TestRunKitInit_EnvVarTakesPrecedence(t *testing.T) {
	withIsolatedConfigHome(t)
	t.Setenv(config.EnvDefaultHarness, "codex")

	in := strings.NewReader("") // should never be read
	var out bytes.Buffer
	if err := runKitInit(in, &out); err != nil {
		t.Fatalf("runKitInit: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "default harness: codex") {
		t.Errorf("output should reflect env override:\n%s", got)
	}
	// Env override should not write to the config file.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.DefaultHarness != "" {
		t.Errorf("config file should be untouched; got DefaultHarness=%q", cfg.DefaultHarness)
	}
}

// kitInitCmd must opt out of EnsureRunning so the first onboarding command
// works before a daemon exists.
func TestKitInitCmd_SkipsAutostart(t *testing.T) {
	if got := kitInitCmd.Annotations[annotationSkipAutostart]; got != "skip" {
		t.Errorf("annotation %q on kit init: got %q, want %q",
			annotationSkipAutostart, got, "skip")
	}
}

