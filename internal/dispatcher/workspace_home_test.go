package dispatcher

import (
	"fmt"
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

// parseEnvDump parses the output of `env` (one KEY=VAL per line, as written
// by the init scripts in this file via `env > $BOID_WORKSPACE_HOME/env-dump`)
// into a key->value map, so callers can assert exact values instead of a
// substring check. A substring check is a false-positive trap here
// specifically: buildWorkspaceInitEnv sets both HOME and
// BOID_WORKSPACE_HOME to the same homeDir, so strings.Contains(content,
// "HOME="+homeDir) would still find a match inside the
// "BOID_WORKSPACE_HOME=<homeDir>" line even if the HOME= line itself were
// silently dropped from the env (codex review, PR #787).
func parseEnvDump(content string) map[string]string {
	env := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[key] = val
	}
	return env
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
	gotEnv := parseEnvDump(content)
	wantEnv := map[string]string{
		"HOME":                homeDir,
		"BOID_WORKSPACE_SLUG": "myws",
		"BOID_WORKSPACE_HOME": homeDir,
	}
	for key, wantVal := range wantEnv {
		gotVal, ok := gotEnv[key]
		if !ok {
			t.Errorf("env-dump missing %s=; got:\n%s", key, content)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("env-dump %s = %q, want %q", key, gotVal, wantVal)
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

// --- 9-11. TOCTOU fix (codex review PR #787): the marker must record the
// hash of the bytes that actually ran, execution must happen from a private
// temp copy rather than re-opening scriptPath by name, and that temp copy
// must not survive the call.

// atomicWriteInitScript rewrites configDir/boid/workspaces/<slug>/init.sh via
// a same-directory temp file + rename, so a concurrent reader (unlike a
// plain os.WriteFile, which truncates before writing) never observes a
// torn/partial write — either the old content or the new content, never a
// half-written mix or empty file.
func atomicWriteInitScript(t *testing.T, configDir, slug, content string) {
	t.Helper()
	dir := filepath.Join(configDir, "boid", "workspaces", slug)
	tmp, err := os.CreateTemp(dir, ".init.sh.*.tmp")
	if err != nil {
		t.Fatalf("create temp init script: %v", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		t.Fatalf("write temp init script: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close temp init script: %v", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "init.sh")); err != nil {
		t.Fatalf("rename temp init script into place: %v", err)
	}
}

// TestResolveWorkspaceHome_TOCTOU_MarkerRecordsExecutedBytesNotLaterRewrite
// pins the TOCTOU fix by construction rather than by racing a goroutine
// against the read (which would be flaky): init.sh, while it runs, rewrites
// itself on disk to a different (v2) script. Because resolveWorkspaceHome
// now re-reads and re-hashes the script under the lock and executes those
// exact bytes from a temp copy — not by re-opening scriptPath after the
// script has already started running — the marker written after the first
// call must record the hash of the original (v1) bytes that were actually
// executed, not whatever v1 rewrote the file to afterward. The on-disk
// content is now v2, which no longer matches the marker's v1 hash, so a
// second call must detect the mismatch and re-run exactly once more.
func TestResolveWorkspaceHome_TOCTOU_MarkerRecordsExecutedBytesNotLaterRewrite(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	scriptPath := filepath.Join(configDir, "boid", "workspaces", "myws", "init.sh")

	v2 := "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n"
	v1 := fmt.Sprintf("#!/bin/bash\ncat > %q <<'V2EOF'\n%sV2EOF\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n", scriptPath, v2)
	writeInitScript(t, configDir, "myws", v1)
	v1Hash := scriptSHA256Hex([]byte(v1), true)
	v2Hash := scriptSHA256Hex([]byte(v2), true)
	if v1Hash == v2Hash {
		t.Fatal("test setup bug: v1 and v2 hashes must differ")
	}

	r := &Runner{}
	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if lines := countLines(t, filepath.Join(homeDir, "counter")); lines != 1 {
		t.Fatalf("counter lines after first call = %d, want 1", lines)
	}

	markerPath := filepath.Join(dataDir, "boid", "homes", "myws.init.json")
	marker, ok, err := readWorkspaceHomeMarker(markerPath)
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}
	if marker.ScriptSHA256 != v1Hash {
		t.Fatalf("marker.ScriptSHA256 = %q, want %q (hash of the v1 bytes actually executed, not the v2 content v1 rewrote itself to)", marker.ScriptSHA256, v1Hash)
	}

	// init.sh on disk is now v2 (self-overwritten during the first run).
	onDisk, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read self-rewritten init.sh: %v", err)
	}
	if string(onDisk) != v2 {
		t.Fatalf("init.sh on disk after first run = %q, want v2 %q", onDisk, v2)
	}

	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if lines := countLines(t, filepath.Join(homeDir, "counter")); lines != 2 {
		t.Errorf("counter lines after second call = %d, want 2 (the on-disk content change during the first run must trigger exactly one re-run)", lines)
	}
	marker, ok, err = readWorkspaceHomeMarker(markerPath)
	if err != nil || !ok {
		t.Fatalf("read marker after second call: ok=%v err=%v", ok, err)
	}
	if marker.ScriptSHA256 != v2Hash {
		t.Errorf("marker.ScriptSHA256 after second call = %q, want %q", marker.ScriptSHA256, v2Hash)
	}
}

// TestResolveWorkspaceHome_TOCTOU_ConcurrentCallsWithMidFlightRewrite races
// 10 concurrent resolveWorkspaceHome("myws") calls against an atomic on-disk
// rewrite of init.sh from a known v1 to a known v2. Whichever content ends
// up recorded in the completion marker must be one of these two snapshots —
// never a corrupted, empty, or otherwise unrelated hash — and the counter
// file (appended once per distinct content actually executed) must reflect
// at most one execution per snapshot.
func TestResolveWorkspaceHome_TOCTOU_ConcurrentCallsWithMidFlightRewrite(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	v1 := "#!/bin/bash\nsleep 0.05\nprintf 'x\\n' >> \"$BOID_WORKSPACE_HOME/counter\"\n"
	v2 := "#!/bin/bash\nprintf 'y\\n' >> \"$BOID_WORKSPACE_HOME/counter\"\n"
	writeInitScript(t, configDir, "myws", v1)
	v1Hash := scriptSHA256Hex([]byte(v1), true)
	v2Hash := scriptSHA256Hex([]byte(v2), true)

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
	// Land the rewrite while the 10 dispatches are in flight (racing the
	// lock, not the read: atomicWriteInitScript's rename means every
	// concurrent reader sees either v1 or v2 in full, never a torn write).
	atomicWriteInitScript(t, configDir, "myws", v2)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	if t.Failed() {
		return
	}

	markerPath := filepath.Join(dataDir, "boid", "homes", "myws.init.json")
	marker, ok, err := readWorkspaceHomeMarker(markerPath)
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}
	if marker.ScriptSHA256 != v1Hash && marker.ScriptSHA256 != v2Hash {
		t.Fatalf("marker.ScriptSHA256 = %q, want either the v1 hash %q or the v2 hash %q (a snapshot that was actually on disk, never a corrupted value)", marker.ScriptSHA256, v1Hash, v2Hash)
	}

	if lines := countLines(t, filepath.Join(homes[0], "counter")); lines < 1 || lines > 2 {
		t.Errorf("counter lines = %d, want 1 or 2 (each distinct on-disk content runs at most once)", lines)
	}
}

// TestResolveWorkspaceHome_TOCTOU_TempInitScriptRemovedAfterRun proves
// execution happens from a private temp file inside homesDir (not scriptPath
// directly) by having the script list homesDir's own (dotfile-inclusive)
// contents mid-run, and then asserts that exact temp file is gone
// (os.Stat -> ENOENT) once resolveWorkspaceHome has returned.
func TestResolveWorkspaceHome_TOCTOU_TempInitScriptRemovedAfterRun(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws",
		"#!/bin/bash\nls -a \"$BOID_WORKSPACE_HOME/..\" | grep 'init\\.sh' > \"$BOID_WORKSPACE_HOME/tmp-script-name\"\n")

	r := &Runner{}
	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	nameBytes, err := os.ReadFile(filepath.Join(homeDir, "tmp-script-name"))
	if err != nil {
		t.Fatalf("read tmp-script-name: %v", err)
	}
	tmpName := strings.TrimSpace(string(nameBytes))
	if tmpName == "" {
		t.Fatal("script did not observe a *.init.sh.*.tmp entry in homesDir while it ran")
	}

	homesDir := filepath.Join(dataDir, "boid", "homes")
	tmpPath := filepath.Join(homesDir, tmpName)
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("temp init script %s still present after run (stat err=%v), want ENOENT", tmpPath, statErr)
	}
}
