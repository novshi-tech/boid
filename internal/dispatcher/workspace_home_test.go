package dispatcher

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file covers resolveWorkspaceHome in isolation (test cases 1-8 of
// docs/plans/home-workspace-volume.md's PR1 test plan). The Dispatch-level
// wiring guards — proof that Runner.Dispatch actually calls
// resolveWorkspaceHome with the right slug, and fails the job correctly when
// it errors — live in workspace_home_dispatch_test.go, which reuses the
// helpers defined here.

// setupWorkspaceHomeTestDirs points HOME/XDG_DATA_HOME/XDG_CONFIG_HOME at
// fresh, isolated t.TempDir()s for the duration of the calling test — a
// stricter, per-test override on top of TestMain's process-wide default (see
// testmain_test.go), so test cases in this file never see each other's
// homes/markers/init scripts.
func setupWorkspaceHomeTestDirs(t *testing.T) (dataDir, configDir string) {
	t.Helper()
	dataDir = t.TempDir()
	configDir = t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	return dataDir, configDir
}

// writeInitScript writes ~/.config/boid/workspaces/<slug>/init.sh (rooted at
// configDir, matching XDG_CONFIG_HOME) with the given content and returns its
// path.
func writeInitScript(t *testing.T, configDir, slug, content string) string {
	t.Helper()
	dir := filepath.Join(configDir, "boid", "workspaces", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir init script dir: %v", err)
	}
	path := filepath.Join(dir, "init.sh")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write init script: %v", err)
	}
	return path
}

// countLines returns the number of non-empty lines in the file at path,
// failing the test if the file cannot be read.
func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	return lines
}

// --- 1. script 無し workspace の素通し ---

func TestResolveWorkspaceHome_NoScript_PassesThrough(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)
	r := &Runner{}

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	wantHome := filepath.Join(dataDir, "boid", "homes", "myws")
	if homeDir != wantHome {
		t.Errorf("homeDir = %q, want %q", homeDir, wantHome)
	}
	if info, statErr := os.Stat(homeDir); statErr != nil || !info.IsDir() {
		t.Fatalf("home dir not created: stat err=%v", statErr)
	}

	markerPath := filepath.Join(dataDir, "boid", "homes", "myws.init.json")
	marker, ok, err := readWorkspaceHomeMarker(markerPath)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if !ok {
		t.Fatal("marker not written for script-less workspace")
	}
	if marker.ScriptSHA256 != "" {
		t.Errorf("ScriptSHA256 = %q, want empty (no init script present)", marker.ScriptSHA256)
	}
	if marker.CompletedAt.IsZero() {
		t.Error("CompletedAt not set on marker")
	}
}

// --- 2. 初回 script 実行 (env 検証込み) ---

func TestResolveWorkspaceHome_FirstRun_ExecutesScriptWithExpectedEnv(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\nenv > \"$BOID_WORKSPACE_HOME/env-dump\"\n")

	r := &Runner{}
	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	dump, err := os.ReadFile(filepath.Join(homeDir, "env-dump"))
	if err != nil {
		t.Fatalf("read env-dump: %v", err)
	}
	content := string(dump)
	for _, want := range []string{
		"HOME=" + homeDir,
		"BOID_WORKSPACE_SLUG=myws",
		"BOID_WORKSPACE_HOME=" + homeDir,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("env-dump missing %q; got:\n%s", want, content)
		}
	}

	markerPath := filepath.Join(dataDir, "boid", "homes", "myws.init.json")
	marker, ok, err := readWorkspaceHomeMarker(markerPath)
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}
	if marker.ScriptSHA256 == "" {
		t.Error("ScriptSHA256 empty, want a non-empty hash for a present init script")
	}
}

// --- 3. 同一 hash の 2 回目呼び出しは素通し ---

func TestResolveWorkspaceHome_UnchangedScript_RunsOnlyOnce(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")

	r := &Runner{}
	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if lines := countLines(t, filepath.Join(homeDir, "counter")); lines != 1 {
		t.Errorf("counter lines = %d, want 1 (script must not re-run for an unchanged script)", lines)
	}
}

// --- 4. script 内容変更で再実行 ---

func TestResolveWorkspaceHome_ScriptContentChanged_ReRuns(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")

	r := &Runner{}
	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Same observable effect, different content -> different hash.
	writeInitScript(t, configDir, "myws", "#!/bin/bash\n# v2\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if lines := countLines(t, filepath.Join(homeDir, "counter")); lines != 2 {
		t.Errorf("counter lines = %d, want 2 (script content change must trigger a re-run)", lines)
	}
}

// --- 5. 並行 dispatch で 1 回だけ実行 ---

func TestResolveWorkspaceHome_ConcurrentCalls_RunExactlyOnce(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	// The sleep widens the race window a concurrency bug would need to slip
	// through; printf >> is an O_APPEND write, so even an accidental
	// double-run would still show up as extra lines rather than corrupting
	// the file.
	writeInitScript(t, configDir, "myws", "#!/bin/bash\nsleep 0.05\nprintf 'x\\n' >> \"$BOID_WORKSPACE_HOME/counter\"\n")

	r := &Runner{}
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	homes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			home, err := r.resolveWorkspaceHome("myws")
			homes[i] = home
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
		if homes[i] != homes[0] {
			t.Errorf("goroutine %d homeDir = %q, want %q (same as goroutine 0)", i, homes[i], homes[0])
		}
	}
	if t.Failed() {
		return
	}

	if lines := countLines(t, filepath.Join(homes[0], "counter")); lines != 1 {
		t.Errorf("counter lines = %d, want exactly 1 across %d concurrent calls", lines, n)
	}
}

// --- 6. script 失敗で明示エラー ---

func TestResolveWorkspaceHome_ScriptFails_ReturnsErrorNoMarkerThenRetries(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho boom >&2\nexit 1\n")

	r := &Runner{}
	if _, err := r.resolveWorkspaceHome("myws"); err == nil {
		t.Fatal("expected an error from a failing init script")
	}

	markerPath := filepath.Join(dataDir, "boid", "homes", "myws.init.json")
	if _, ok, err := readWorkspaceHomeMarker(markerPath); err != nil || ok {
		t.Fatalf("marker must not be written on init failure: ok=%v err=%v", ok, err)
	}

	// Still failing -> still an error on retry (no silent "already tried"
	// caching of the failure).
	if _, err := r.resolveWorkspaceHome("myws"); err == nil {
		t.Fatal("expected an error on retry while the script still fails")
	}

	// Fix the script; the next call must succeed and write the marker.
	writeInitScript(t, configDir, "myws", "#!/bin/bash\nexit 0\n")
	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("expected success after fixing the script: %v", err)
	}
	if _, ok, err := readWorkspaceHomeMarker(markerPath); err != nil || !ok {
		t.Fatalf("marker should be written after a successful retry: ok=%v err=%v", ok, err)
	}
}

// --- 7. workspace slug 正規化 ("" -> default) ---

func TestResolveWorkspaceHome_EmptyWorkspaceID_UsesDefaultSlug(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)
	r := &Runner{}

	homeDir, err := r.resolveWorkspaceHome("")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	want := filepath.Join(dataDir, "boid", "homes", orchestrator.DefaultWorkspaceSlug)
	if homeDir != want {
		t.Errorf("homeDir = %q, want %q", homeDir, want)
	}
}

// --- 8. slug 検証 ---

func TestResolveWorkspaceHome_InvalidSlug_ReturnsError(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := &Runner{}

	for _, bad := range []string{"../etc", "a b", "Has/Slash", "UPPERCASE", strings.Repeat("x", 65)} {
		if _, err := r.resolveWorkspaceHome(bad); err == nil {
			t.Errorf("resolveWorkspaceHome(%q) = nil error, want error", bad)
		}
	}
}
