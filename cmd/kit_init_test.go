package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/config"
)

// writeKitYAML creates kitsDir/<kitName>/kit.yaml with the given content.
// It is used by secret-scan tests to simulate files written by the sandbox.
func writeKitYAML(t *testing.T, kitsDir, kitName, content string) string {
	t.Helper()
	dir := filepath.Join(kitsDir, kitName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "kit.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

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

// withStubSandboxLaunch replaces kitInitExecFn with a no-op stub that records
// the argv it receives and returns nil. Restored automatically via t.Cleanup.
// Returns a pointer to the captured argv0 so tests can assert it.
func withStubSandboxLaunch(t *testing.T) *string {
	t.Helper()
	captured := ""
	orig := kitInitExecFn
	kitInitExecFn = func(argv0 string, argv []string, envv []string) error {
		captured = argv0
		return nil
	}
	t.Cleanup(func() { kitInitExecFn = orig })
	return &captured
}

// withIsolatedDataHome routes XDG_DATA_HOME to a per-test tempdir so
// skills.DeployAll and kits-dir creation land in the temp tree.
func withIsolatedDataHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	return dir
}

func TestRunKitInit_PromptsAndPersists(t *testing.T) {
	dir := withIsolatedConfigHome(t)
	withIsolatedDataHome(t)
	withStubSandboxLaunch(t)

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
	withIsolatedDataHome(t)
	withStubSandboxLaunch(t)

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
	withIsolatedDataHome(t)
	withStubSandboxLaunch(t)

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
	// No stub for exec fn — we expect the function to return an error before
	// reaching the sandbox launch step.

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
	// No stub for exec fn — we expect the function to return an error before
	// reaching the sandbox launch step.

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
	withIsolatedDataHome(t)
	withStubSandboxLaunch(t)

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

// TestRunKitInit_ExecFnCalled verifies that runKitInit reaches the sandbox
// launch step (kitInitExecFn is called) when the harness is already configured.
func TestRunKitInit_ExecFnCalled(t *testing.T) {
	withIsolatedConfigHome(t)
	dataDir := withIsolatedDataHome(t)

	if err := config.SetDefaultHarness("claude"); err != nil {
		t.Fatalf("SetDefaultHarness: %v", err)
	}

	var gotArgv0 string
	orig := kitInitExecFn
	kitInitExecFn = func(argv0 string, argv []string, envv []string) error {
		gotArgv0 = argv0
		return nil
	}
	t.Cleanup(func() { kitInitExecFn = orig })

	in := strings.NewReader("")
	var out bytes.Buffer
	if err := runKitInit(in, &out); err != nil {
		t.Fatalf("runKitInit: %v", err)
	}

	if gotArgv0 == "" {
		t.Error("kitInitExecFn was not called — sandbox launch did not happen")
	}

	// Skills should have been deployed to the data dir.
	skillsDir := filepath.Join(dataDir, "boid", "skills")
	if _, err := os.Stat(filepath.Join(skillsDir, "boid-sandbox-configure", "SKILL.md")); err != nil {
		t.Errorf("boid-sandbox-configure skill not deployed: %v", err)
	}
}

// TestRunKitInit_ExecFnError verifies that errors from kitInitExecFn are
// propagated back to the caller.
func TestRunKitInit_ExecFnError(t *testing.T) {
	withIsolatedConfigHome(t)
	withIsolatedDataHome(t)

	if err := config.SetDefaultHarness("claude"); err != nil {
		t.Fatalf("SetDefaultHarness: %v", err)
	}

	wantErr := errors.New("exec failed")
	orig := kitInitExecFn
	kitInitExecFn = func(argv0 string, argv []string, envv []string) error {
		return wantErr
	}
	t.Cleanup(func() { kitInitExecFn = orig })

	in := strings.NewReader("")
	var out bytes.Buffer
	err := runKitInit(in, &out)
	if err == nil {
		t.Fatal("expected error from kitInitExecFn to propagate")
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

// ---------------------------------------------------------------------------
// Secret scan / rollback tests (PR5)
// ---------------------------------------------------------------------------

// TestScanNewKitDirs_CleanYAML verifies that a newly generated kit dir with
// clean (no-secret) YAML is accepted, and the function reports the kit name.
func TestScanNewKitDirs_CleanYAML(t *testing.T) {
	kitsDir := t.TempDir()

	// Pre-existing kit (must NOT be scanned or removed).
	writeKitYAML(t, kitsDir, "old-kit", "name: old-kit\ntools: []\n")
	existing, err := listKitDirs(kitsDir)
	if err != nil {
		t.Fatalf("listKitDirs: %v", err)
	}

	// New kit with clean YAML — no secrets.
	writeKitYAML(t, kitsDir, "new-kit", "name: new-kit\ntools:\n  - name: hello\n    cmd: echo hello\n")

	var out bytes.Buffer
	if err := scanNewKitDirs(kitsDir, existing, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "new-kit") {
		t.Errorf("output should mention generated kit; got: %s", out.String())
	}

	// old-kit and new-kit must both still exist.
	for _, name := range []string{"old-kit", "new-kit"} {
		if _, err := os.Stat(filepath.Join(kitsDir, name)); err != nil {
			t.Errorf("kit dir %q should still exist: %v", name, err)
		}
	}
}

// TestScanNewKitDirs_SecretYAMLRollback verifies that when the generated kit
// dir contains a secret-like value the directory is deleted and an error is
// returned (rollback).
func TestScanNewKitDirs_SecretYAMLRollback(t *testing.T) {
	kitsDir := t.TempDir()

	existing, err := listKitDirs(kitsDir)
	if err != nil {
		t.Fatalf("listKitDirs: %v", err)
	}

	// Write a kit yaml with a high-entropy token (≥32 chars of [A-Za-z0-9_-]).
	secretToken := "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 40 chars, token-like
	writeKitYAML(t, kitsDir, "leaked-kit",
		"name: leaked-kit\nenv:\n  GITHUB_TOKEN: "+secretToken+"\n")

	var out bytes.Buffer
	err = scanNewKitDirs(kitsDir, existing, &out)
	if err == nil {
		t.Fatal("expected error due to secret finding, got nil")
	}
	if !strings.Contains(err.Error(), "secret scan") {
		t.Errorf("error should mention secret scan; got: %v", err)
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error should mention rollback; got: %v", err)
	}

	// The leaked kit directory must be gone (rollback).
	if _, statErr := os.Stat(filepath.Join(kitsDir, "leaked-kit")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("leaked-kit dir should have been removed by rollback")
	}
}

// TestScanNewKitDirs_ExistingKitNotScanned verifies that pre-existing kit
// directories (present before the sandbox run) are never scanned, even when
// they contain secret-like content.
func TestScanNewKitDirs_ExistingKitNotScanned(t *testing.T) {
	kitsDir := t.TempDir()

	// Write a pre-existing kit with a secret-like value.
	secretToken := "ghp_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	writeKitYAML(t, kitsDir, "pre-existing-kit",
		"name: pre-existing-kit\nenv:\n  TOKEN: "+secretToken+"\n")

	// Snapshot — pre-existing-kit is now in the baseline.
	existing, err := listKitDirs(kitsDir)
	if err != nil {
		t.Fatalf("listKitDirs: %v", err)
	}

	// Add a clean new kit after the snapshot.
	writeKitYAML(t, kitsDir, "new-clean-kit", "name: new-clean-kit\ntools: []\n")

	var out bytes.Buffer
	if err := scanNewKitDirs(kitsDir, existing, &out); err != nil {
		t.Fatalf("pre-existing kit should not cause scan failure: %v", err)
	}

	// pre-existing-kit must remain intact.
	if _, statErr := os.Stat(filepath.Join(kitsDir, "pre-existing-kit")); statErr != nil {
		t.Errorf("pre-existing-kit should not be touched: %v", statErr)
	}
	if !strings.Contains(out.String(), "new-clean-kit") {
		t.Errorf("output should mention new-clean-kit; got: %s", out.String())
	}
}

// TestRunKitInit_SecretScanRollback verifies the end-to-end integration: when
// the sandbox stub writes a secret-containing YAML to a new kit dir, runKitInit
// returns an error and the kit dir is rolled back.
func TestRunKitInit_SecretScanRollback(t *testing.T) {
	withIsolatedConfigHome(t)
	dataDir := withIsolatedDataHome(t)

	if err := config.SetDefaultHarness("claude"); err != nil {
		t.Fatalf("SetDefaultHarness: %v", err)
	}

	kitsDir := filepath.Join(dataDir, "boid", "kits")

	orig := kitInitExecFn
	kitInitExecFn = func(argv0 string, argv []string, envv []string) error {
		// Simulate the sandbox writing a kit yaml with a secret token.
		secretToken := "ghp_CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
		writeKitYAML(t, kitsDir, "sandbox-generated-kit",
			"name: sandbox-generated-kit\nenv:\n  SECRET: "+secretToken+"\n")
		return nil
	}
	t.Cleanup(func() { kitInitExecFn = orig })

	in := strings.NewReader("")
	var out bytes.Buffer
	err := runKitInit(in, &out)
	if err == nil {
		t.Fatal("expected error from secret scan, got nil")
	}
	if !strings.Contains(err.Error(), "secret scan") {
		t.Errorf("error should mention secret scan; got: %v", err)
	}

	// The generated kit dir must have been rolled back.
	if _, statErr := os.Stat(filepath.Join(kitsDir, "sandbox-generated-kit")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("sandbox-generated-kit should have been rolled back")
	}
}

// TestRunKitInit_SecretScanClean verifies that when the sandbox writes clean
// YAML, runKitInit succeeds and reports the generated kit name.
func TestRunKitInit_SecretScanClean(t *testing.T) {
	withIsolatedConfigHome(t)
	dataDir := withIsolatedDataHome(t)

	if err := config.SetDefaultHarness("claude"); err != nil {
		t.Fatalf("SetDefaultHarness: %v", err)
	}

	kitsDir := filepath.Join(dataDir, "boid", "kits")

	orig := kitInitExecFn
	kitInitExecFn = func(argv0 string, argv []string, envv []string) error {
		writeKitYAML(t, kitsDir, "clean-kit", "name: clean-kit\ntools: []\n")
		return nil
	}
	t.Cleanup(func() { kitInitExecFn = orig })

	in := strings.NewReader("")
	var out bytes.Buffer
	if err := runKitInit(in, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "clean-kit") {
		t.Errorf("output should mention clean-kit; got: %s", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(kitsDir, "clean-kit")); statErr != nil {
		t.Errorf("clean-kit dir should still exist: %v", statErr)
	}
}
