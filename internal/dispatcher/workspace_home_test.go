package dispatcher

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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

// --- 0. WorkspaceHomesDir (exported free function, Phase 4 PR5) ---

// TestWorkspaceHomesDir_DerivesFromRuntimesDir pins the primary path: given
// a non-empty runtimesDir (server/wire.go's runtimesDirFor(cfg)), the homes
// dir is its sibling "homes" directory, not a subdirectory of it.
func TestWorkspaceHomesDir_DerivesFromRuntimesDir(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	runtimesDir := filepath.Join(t.TempDir(), "data", "runtimes")

	got, err := WorkspaceHomesDir(runtimesDir)
	if err != nil {
		t.Fatalf("WorkspaceHomesDir: %v", err)
	}
	want := filepath.Join(filepath.Dir(runtimesDir), "homes")
	if got != want {
		t.Errorf("WorkspaceHomesDir(%q) = %q, want %q", runtimesDir, got, want)
	}
}

// TestWorkspaceHomesDir_FallsBackToXDGDataHome pins the secondary path: an
// empty runtimesDir (e.g. minimal test wiring, or a daemon build that never
// set RuntimesDir) falls back to $XDG_DATA_HOME/boid/homes.
func TestWorkspaceHomesDir_FallsBackToXDGDataHome(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)

	got, err := WorkspaceHomesDir("")
	if err != nil {
		t.Fatalf("WorkspaceHomesDir: %v", err)
	}
	want := filepath.Join(dataDir, "boid", "homes")
	if got != want {
		t.Errorf("WorkspaceHomesDir(\"\") = %q, want %q", got, want)
	}
}

// TestRunnerWorkspaceHomesDir_MatchesFreeFunction pins that the *Runner
// method is now a pure delegation to the exported free function — no
// behavior drift from the PR5 export refactor.
func TestRunnerWorkspaceHomesDir_MatchesFreeFunction(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := &Runner{RuntimesDir: filepath.Join(t.TempDir(), "data", "runtimes")}

	got, err := r.workspaceHomesDir()
	if err != nil {
		t.Fatalf("r.workspaceHomesDir: %v", err)
	}
	want, err := WorkspaceHomesDir(r.RuntimesDir)
	if err != nil {
		t.Fatalf("WorkspaceHomesDir: %v", err)
	}
	if got != want {
		t.Errorf("r.workspaceHomesDir() = %q, want %q (WorkspaceHomesDir(r.RuntimesDir))", got, want)
	}
}

// --- 0-b. workspaceHomeMetaDir (docs/plans/workspace-home-volume-persistence.md
// 論点b, PR2) — marker/lock moved off the (soon-to-be-a-named-volume) homes/
// root onto the daemon's OWN persistent data root. ---

// TestWorkspaceHomeMetaDir_DerivesFromDataHome pins the primary path: given a
// non-empty dataHome (server/wire.go's dataHomeFor(cfg) — the directory
// boid.db / web_secret / install_id already live in, i.e. the `boid_state`
// volume under the container deploy), the meta dir is its homes-meta/ child.
func TestWorkspaceHomeMetaDir_DerivesFromDataHome(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	dataHome := filepath.Join(t.TempDir(), "state")

	got, err := workspaceHomeMetaDir(dataHome)
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	want := filepath.Join(dataHome, "homes-meta")
	if got != want {
		t.Errorf("workspaceHomeMetaDir(%q) = %q, want %q", dataHome, got, want)
	}
}

// TestWorkspaceHomeMetaDir_FallsBackToXDGDataHome pins the fallback that
// keeps a bare &Runner{} (minimal test wiring, or a daemon build that never
// set DataHomeDir) off the real developer machine's $HOME — the same reason
// WorkspaceHomesDir carries a fallback of exactly this shape.
func TestWorkspaceHomeMetaDir_FallsBackToXDGDataHome(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)

	got, err := workspaceHomeMetaDir("")
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	want := filepath.Join(dataDir, "boid", "homes-meta")
	if got != want {
		t.Errorf("workspaceHomeMetaDir(\"\") = %q, want %q", got, want)
	}
}

// TestRunnerWorkspaceHomeMetaDir_MatchesFreeFunction mirrors
// TestRunnerWorkspaceHomesDir_MatchesFreeFunction: the *Runner method is a
// pure delegation to the free function over r.DataHomeDir.
func TestRunnerWorkspaceHomeMetaDir_MatchesFreeFunction(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := &Runner{DataHomeDir: filepath.Join(t.TempDir(), "state")}

	got, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("r.workspaceHomeMetaDir: %v", err)
	}
	want, err := workspaceHomeMetaDir(r.DataHomeDir)
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	if got != want {
		t.Errorf("r.workspaceHomeMetaDir() = %q, want %q (workspaceHomeMetaDir(r.DataHomeDir))", got, want)
	}
}

// TestWire_DataHomeDir_ReachesMarkerAndLockOnDisk is the wiring-seam guard
// for the ONE new configuration edge PR2 introduces
// (docs/plans/workspace-home-volume-persistence.md 論点b): server/wire.go's
// dataHomeFor(cfg) -> dispatcher.WireConfig.DataHomeDir -> Runner.DataHomeDir
// -> the marker/lock files resolveWorkspaceHome actually writes. Runner had
// no notion of the daemon's persistent data root before this PR (only
// RuntimesDir), so this is exactly the "one end changes, the other silently
// drifts" class the boid-review skill's wiring-seam doctrine targets — a unit
// test of workspaceHomeMetaDir alone would still pass if Wire dropped the
// field, or if resolveWorkspaceHome kept resolving marker/lock off homesDir.
//
// DataHomeDir and RuntimesDir deliberately point at UNRELATED temp roots so
// the assertion cannot be satisfied by accident: the home directory must land
// under the RuntimesDir-derived homes/, and the marker+lock must land under
// the DataHomeDir-derived homes-meta/, with neither leaking into the other.
func TestWire_DataHomeDir_ReachesMarkerAndLockOnDisk(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	dataHome := filepath.Join(t.TempDir(), "state")
	runtimesDir := filepath.Join(t.TempDir(), "runtime", "runtimes")

	r := Wire(WireConfig{DataHomeDir: dataHome, RuntimesDir: runtimesDir})
	if r.DataHomeDir != dataHome {
		t.Fatalf("Wire dropped DataHomeDir: got %q, want %q", r.DataHomeDir, dataHome)
	}

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	wantHome := filepath.Join(filepath.Dir(runtimesDir), "homes", "myws")
	if homeDir != wantHome {
		t.Errorf("homeDir = %q, want %q (still derived from RuntimesDir — PR2 moves marker/lock only)", homeDir, wantHome)
	}

	metaDir := filepath.Join(dataHome, "homes-meta")
	for _, name := range []string{"myws.init.json", "myws.lock"} {
		if _, statErr := os.Stat(filepath.Join(metaDir, name)); statErr != nil {
			t.Errorf("%s not found under the DataHomeDir-derived meta dir %s: %v", name, metaDir, statErr)
		}
	}

	// ...and nothing marker/lock-shaped was left behind next to the homes.
	entries, err := os.ReadDir(filepath.Dir(homeDir))
	if err != nil {
		t.Fatalf("read homes dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "myws" {
			t.Errorf("unexpected entry %q under homes/ — marker/lock must no longer live beside workspace homes", e.Name())
		}
	}
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

	markerPath := filepath.Join(dataDir, "boid", "homes-meta", "myws.init.json")
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

	markerPath := filepath.Join(dataDir, "boid", "homes-meta", "myws.init.json")
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

	markerPath := filepath.Join(dataDir, "boid", "homes-meta", "myws.init.json")
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

	markerPath := filepath.Join(dataDir, "boid", "homes-meta", "myws.init.json")
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

// TestResolveWorkspaceHome_TOCTOU_ExecutesTheSnapshotReadUnderTheLock is the
// DETERMINISTIC pin for the re-read half of the PR #787 fix: the snapshot
// that gets executed and recorded is the one read AFTER the lock was taken,
// not the one read before it. The concurrent test below cannot pin that (it
// has no way to order the on-disk rewrite against an unobservable read
// inside a goroutine, so an interleaving where everyone reads the new
// content from the start satisfies it too — codex review 2nd round), so this
// test constructs the ordering instead of racing for it. Both required
// orderings are enforced, not hoped for:
//
//   - "the pre-lock read happened, and it returned v1": init.sh is a FIFO,
//     so the pre-lock read BLOCKS until this test opens the write end. Our
//     open returning is proof the reader is there; our close is what ends
//     its read. There is no timing assumption in either direction.
//   - "the rewrite lands after that read and before the under-lock read":
//     this test holds the workspace's own flock for the whole window, so the
//     call under test physically cannot reach its second read until we
//     release — whatever the scheduler does.
//
// With the re-read removed (or kept but with the stale bytes handed to
// runWorkspaceInitScript), v1 runs and v1's hash is recorded, and both
// assertions below fail. Mutation-verified.
func TestResolveWorkspaceHome_TOCTOU_ExecutesTheSnapshotReadUnderTheLock(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)

	v1 := "#!/bin/bash\nprintf 'v1\\n' >> \"$BOID_WORKSPACE_HOME/executed\"\n"
	v2 := "#!/bin/bash\nprintf 'v2\\n' >> \"$BOID_WORKSPACE_HOME/executed\"\n"
	v2Hash := scriptSHA256Hex([]byte(v2), true)

	scriptDir := filepath.Join(configDir, "boid", "workspaces", "myws")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir init script dir: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "init.sh")
	if err := syscall.Mkfifo(scriptPath, 0o600); err != nil {
		t.Fatalf("mkfifo init.sh: %v", err)
	}

	r := &Runner{}
	metaDir := filepath.Join(dataDir, "boid", "homes-meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir meta dir: %v", err)
	}
	release, err := acquireWorkspaceHomeLock(workspaceHomeLockPath(metaDir, "myws"))
	if err != nil {
		t.Fatalf("acquire the workspace lock from the test side: %v", err)
	}
	var releaseOnce sync.Once
	releaseLock := func() { releaseOnce.Do(release) }
	defer releaseLock()

	type result struct {
		home string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		home, err := r.resolveWorkspaceHome("myws")
		done <- result{home, err}
	}()

	// Rendezvous: opening the write end of a FIFO returns only once a reader
	// has opened the read end, i.e. once resolveWorkspaceHome has reached its
	// pre-lock read of init.sh. Serve v1 there and nowhere else.
	served := make(chan error, 1)
	go func() {
		w, err := os.OpenFile(scriptPath, os.O_WRONLY, 0)
		if err != nil {
			served <- err
			return
		}
		_, err = io.WriteString(w, v1)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve v1 over the init.sh FIFO: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolveWorkspaceHome never performed a read of init.sh before taking the lock within 10s")
	}

	// The call under test now holds v1 as its pre-lock snapshot and is stuck
	// on the flock this test owns. Replace the FIFO with an ordinary v2 file;
	// nothing can observe the intermediate states because of that lock.
	if err := os.Remove(scriptPath); err != nil {
		t.Fatalf("remove the init.sh FIFO: %v", err)
	}
	if err := os.WriteFile(scriptPath, []byte(v2), 0o644); err != nil {
		t.Fatalf("install v2: %v", err)
	}

	releaseLock()

	var res result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("resolveWorkspaceHome did not finish within 30s of the lock being released")
	}
	if res.err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", res.err)
	}

	executed, err := os.ReadFile(filepath.Join(res.home, "executed"))
	if err != nil {
		t.Fatalf("read the executed-versions log: %v", err)
	}
	if got := string(executed); got != "v2\n" {
		t.Errorf("executed %q, want \"v2\\n\" — the script that runs must be the snapshot read AFTER the lock was acquired, not the pre-lock one", got)
	}

	marker, ok, err := readWorkspaceHomeMarker(workspaceHomeMarkerPath(metaDir, "myws"))
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}
	if marker.ScriptSHA256 != v2Hash {
		t.Errorf("marker.ScriptSHA256 = %q, want the v2 hash %q — the recorded hash must be the one of the bytes that ran", marker.ScriptSHA256, v2Hash)
	}
}

// TestResolveWorkspaceHome_TOCTOU_ConcurrentCallsWithMidFlightRewrite races
// 10 concurrent resolveWorkspaceHome("myws") calls against an atomic on-disk
// rewrite of init.sh from a known v1 to a known v2. This is the stress guard
// for the invariant — the deterministic pin for "the under-lock snapshot is
// the one that runs" is the test above — so its assertions are on the exact
// sequence of contents that executed, not on a line count: each version
// appends its own tag, so ["v1","v2"] and ["v1"] and ["v2"] are the only
// legal logs. Counting alone (the pre-review version of this test allowed
// "1 or 2 lines") would accept the SAME snapshot running twice, which is the
// very thing the lock exists to prevent.
//
// Reversed or repeated tags are impossible by construction, which is what
// makes this assertable: under-lock reads are serialized, so once v2 is on
// disk no later reader can see v1 again, and each snapshot that runs writes
// a marker that makes the next holder of the lock skip it.
func TestResolveWorkspaceHome_TOCTOU_ConcurrentCallsWithMidFlightRewrite(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	v1 := "#!/bin/bash\nsleep 0.05\nprintf 'v1\\n' >> \"$BOID_WORKSPACE_HOME/executed\"\n"
	v2 := "#!/bin/bash\nprintf 'v2\\n' >> \"$BOID_WORKSPACE_HOME/executed\"\n"
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

	markerPath := filepath.Join(dataDir, "boid", "homes-meta", "myws.init.json")
	marker, ok, err := readWorkspaceHomeMarker(markerPath)
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}

	raw, err := os.ReadFile(filepath.Join(homes[0], "executed"))
	if err != nil {
		t.Fatalf("read the executed-versions log: %v", err)
	}
	executed := string(raw)
	var wantMarkerHash string
	switch executed {
	case "v1\n":
		wantMarkerHash = v1Hash
	case "v2\n", "v1\nv2\n":
		wantMarkerHash = v2Hash
	default:
		t.Fatalf("executed %q, want exactly one of \"v1\\n\", \"v2\\n\", \"v1\\nv2\\n\" — every distinct snapshot runs at most once, and never out of order", executed)
	}
	if marker.ScriptSHA256 != wantMarkerHash {
		t.Errorf("marker.ScriptSHA256 = %q, want %q — the marker must record the hash of the last snapshot that actually ran (executed log: %q)", marker.ScriptSHA256, wantMarkerHash, executed)
	}
}

// TestResolveWorkspaceHome_TOCTOU_TempInitScriptRemovedAfterRun proves
// execution happens from a private temp file (not scriptPath directly) and
// that the temp file is gone (os.Stat -> ENOENT) once resolveWorkspaceHome
// has returned. The script reports its own $0 — which, per
// runWorkspaceInitScript's documented trade-off, is the temp copy's path
// rather than the configured init.sh location — so the check needs no
// guesswork about the temp file's name.
//
// The temp file's DIRECTORY is asserted too, and that is the part PR2
// changed (docs/plans/workspace-home-volume-persistence.md 論点b): it used to
// be homesDir, which runWorkspaceInitScript's own doc comment justified as
// "already lock-serialized and isolated from any sandbox mount". Moving the
// lock to homes-meta/ would have left that justification false had the temp
// file stayed behind in homesDir, so the temp file moved with the lock.
// $0 pointing back at scriptPath, or at anything outside the meta dir, is a
// regression of the PR #787 TOCTOU fix and/or of that isolation property.
func TestResolveWorkspaceHome_TOCTOU_TempInitScriptRemovedAfterRun(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	scriptPath := writeInitScript(t, configDir, "myws",
		"#!/bin/bash\nprintf '%s\\n' \"$0\" > \"$BOID_WORKSPACE_HOME/tmp-script-path\"\n")

	r := &Runner{}
	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	pathBytes, err := os.ReadFile(filepath.Join(homeDir, "tmp-script-path"))
	if err != nil {
		t.Fatalf("read tmp-script-path: %v", err)
	}
	tmpPath := strings.TrimSpace(string(pathBytes))
	if tmpPath == "" {
		t.Fatal("script did not report its own $0")
	}
	if tmpPath == scriptPath {
		t.Fatalf("init.sh ran as %s — the configured script path itself, not a private temp copy (TOCTOU fix, PR #787)", tmpPath)
	}

	metaDir := filepath.Join(dataDir, "boid", "homes-meta")
	if got := filepath.Dir(tmpPath); got != metaDir {
		t.Errorf("temp init script lived in %s, want the lock-serialized meta dir %s (runWorkspaceInitScript's isolation premise)", got, metaDir)
	}

	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("temp init script %s still present after run (stat err=%v), want ENOENT", tmpPath, statErr)
	}
}

// --- 12-16. marker と実体の identity 突合 (nonce) ---
//
// docs/plans/workspace-home-volume-persistence.md 論点b. PR2 moved the
// completion marker into the daemon's own persistent data root while the
// workspace home itself stayed derived from RuntimesDir. Those are two
// different lifetimes now (the marker survives a host reboot inside the
// boid_state volume; the home does not, since BOID_RUNTIME_DIR is tmpfs), so
// "the marker says initialized" stopped implying "the home is initialized".
// A nonce written into the home and recorded in the marker is what re-ties
// them together.

// splitRootRunner returns a Runner wired the way production wires one after
// PR2: the marker/lock root (DataHomeDir) and the workspace home root
// (RuntimesDir) are deliberately UNRELATED temp roots, so a test can destroy
// one without touching the other — which is precisely the situation a host
// reboot creates on a container-backend deploy.
func splitRootRunner(t *testing.T) *Runner {
	t.Helper()
	return &Runner{
		DataHomeDir: filepath.Join(t.TempDir(), "state"),
		RuntimesDir: filepath.Join(t.TempDir(), "runtime", "runtimes"),
	}
}

// TestResolveWorkspaceHome_HomeWipedWhileMarkerSurvives_ReRunsInit is the
// regression guard for the failure mode 論点b predicts: the workspace home
// vanishes (host reboot clears BOID_RUNTIME_DIR's tmpfs, a stray `docker
// volume rm`, a reap misfire) while the marker in the daemon's own persistent
// root survives, so the next dispatch skips init.sh and hands the job an
// EMPTY $HOME. The error that surfaces from there is the adapter's "CLI not
// found" (internal/adapters/claude/run.go), which points at an unconfigured
// init.sh rather than at the real cause.
func TestResolveWorkspaceHome_HomeWipedWhileMarkerSurvives_ReRunsInit(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")
	r := splitRootRunner(t)

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if lines := countLines(t, filepath.Join(homeDir, "counter")); lines != 1 {
		t.Fatalf("counter lines after first call = %d, want 1", lines)
	}

	// Host reboot: the home's root is tmpfs, the marker's root is not.
	if err := os.RemoveAll(homeDir); err != nil {
		t.Fatalf("wipe home dir: %v", err)
	}
	markerPath := workspaceHomeMarkerPath(filepath.Join(r.DataHomeDir, "homes-meta"), "myws")
	if _, ok, err := readWorkspaceHomeMarker(markerPath); err != nil || !ok {
		t.Fatalf("test setup bug: the marker must have survived the wipe: ok=%v err=%v", ok, err)
	}

	homeDir2, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if homeDir2 != homeDir {
		t.Fatalf("homeDir changed across calls: %q -> %q", homeDir, homeDir2)
	}
	counter := filepath.Join(homeDir2, "counter")
	if _, statErr := os.Stat(counter); statErr != nil {
		t.Fatalf("init.sh was skipped for a wiped home: %v — the surviving marker must not be trusted on its own (docs/plans/workspace-home-volume-persistence.md 論点b)", statErr)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Errorf("counter lines after re-init = %d, want 1 (exactly one re-run against the fresh home)", lines)
	}

	// ...and the re-init must be one-shot: a third call sees a home whose
	// nonce now matches the (rewritten) marker again and skips.
	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Errorf("counter lines after third call = %d, want 1 (the identity check must re-settle, not re-run on every dispatch)", lines)
	}
}

// TestResolveWorkspaceHome_NoScript_HomeWipedWhileMarkerSurvives_ReSeedsNonce
// covers the pass-through class: a workspace with no init.sh still gets a
// marker written (docs/plans/home-workspace-volume.md「script が無い workspace
// は素通し (マーカーだけ打つ)」), so it needs a nonce for exactly the same
// reason — otherwise the identity check simply does not apply to that class
// and the desync window stays open for it.
func TestResolveWorkspaceHome_NoScript_HomeWipedWhileMarkerSurvives_ReSeedsNonce(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := splitRootRunner(t)

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	noncePath := filepath.Join(homeDir, workspaceHomeNonceFileName)
	first, err := os.ReadFile(noncePath)
	if err != nil {
		t.Fatalf("a script-less workspace must still get a nonce written into its home: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("nonce file is empty")
	}

	if err := os.RemoveAll(homeDir); err != nil {
		t.Fatalf("wipe home dir: %v", err)
	}
	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, err := os.ReadFile(noncePath)
	if err != nil {
		t.Fatalf("nonce not re-seeded after the home was wiped: %v", err)
	}
	if string(second) == string(first) {
		t.Error("re-seeded nonce equals the pre-wipe one — a fresh init must mint a fresh identity, otherwise a home restored from an old backup would masquerade as the current one")
	}
}

// TestResolveWorkspaceHome_MarkerWithoutHomeID_ReInitializes pins the
// fail-safe direction for markers written by a pre-nonce build: they carry no
// identity to compare against, so they must be treated as "unknown", not as
// "verified". The home here is fully populated and even has a nonce file —
// only the marker's record of it is missing — so a build that skipped on the
// hash alone would pass this test.
func TestResolveWorkspaceHome_MarkerWithoutHomeID_ReInitializes(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	script := "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n"
	writeInitScript(t, configDir, "myws", script)
	r := splitRootRunner(t)

	homesDir, err := r.workspaceHomesDir()
	if err != nil {
		t.Fatalf("workspaceHomesDir: %v", err)
	}
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	homeDir := filepath.Join(homesDir, "myws")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, workspaceHomeNonceFileName), []byte("some-old-nonce"), 0o600); err != nil {
		t.Fatalf("seed nonce: %v", err)
	}
	// A pre-nonce daemon's marker: correct hash, no identity field at all.
	if err := writeWorkspaceHomeMarker(workspaceHomeMarkerPath(metaDir, "myws"), workspaceHomeMarker{
		ScriptSHA256: scriptSHA256Hex([]byte(script), true),
		CompletedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}

	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(homeDir, "counter")); statErr != nil {
		t.Fatalf("init.sh was skipped on a marker with no home_id: %v — a marker that records no identity cannot vouch for the home (fail-safe: re-init)", statErr)
	}
}

// TestResolveWorkspaceHome_NonceTamperedInsideHome_ReInitializes pins the
// direction a sandboxed job CAN push this in. The nonce lives inside the
// workspace home, which a job gets an rw mount of, so a job can delete or
// rewrite it — but it cannot read the marker's copy, so the only reachable
// outcome is a redundant re-init of a contractually idempotent script, never
// a spurious skip. Pinning it keeps that asymmetry from silently inverting.
func TestResolveWorkspaceHome_NonceTamperedInsideHome_ReInitializes(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")
	r := splitRootRunner(t)

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	counter := filepath.Join(homeDir, "counter")
	if lines := countLines(t, counter); lines != 1 {
		t.Fatalf("counter lines after first call = %d, want 1", lines)
	}

	if err := os.WriteFile(filepath.Join(homeDir, workspaceHomeNonceFileName), []byte("forged-by-a-job"), 0o600); err != nil {
		t.Fatalf("tamper with nonce: %v", err)
	}
	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if lines := countLines(t, counter); lines != 2 {
		t.Errorf("counter lines after tampering = %d, want 2 (a nonce that does not match the marker must force a re-init)", lines)
	}
}

// TestResolveWorkspaceHome_NonceIsPerHomeAndUnpredictable pins what the
// identity check leans on: every init mints a FRESH random token, so the
// value identifies one incarnation of one home rather than the home's name.
//
// "Differs between two workspaces" and "is long" are not enough on their own
// — a deterministic derivation such as sha256(slug) satisfies both, and the
// pre-review version of this test accepted exactly that (verified by
// mutation). What rules it out is re-minting the SAME slug: a derived value
// would come back identical, and then a home restored from an old backup of
// that workspace — or one a job re-stamped with a value it recorded earlier —
// would present a matching identity for a home that is not the one the
// marker describes.
func TestResolveWorkspaceHome_NonceIsPerHomeAndUnpredictable(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := splitRootRunner(t)

	read := func(slug string) (homeDir, nonce string) {
		t.Helper()
		homeDir, err := r.resolveWorkspaceHome(slug)
		if err != nil {
			t.Fatalf("resolveWorkspaceHome(%q): %v", slug, err)
		}
		data, err := os.ReadFile(filepath.Join(homeDir, workspaceHomeNonceFileName))
		if err != nil {
			t.Fatalf("read nonce for %q: %v", slug, err)
		}
		return homeDir, string(data)
	}

	homeA, a := read("ws-a")
	_, b := read("ws-b")
	if a == b {
		t.Errorf("both workspaces got the same nonce %q — the identity must be per-home", a)
	}
	// newWorkspaceHomeNonce's stated format: 32 bytes, hex-encoded.
	if want := 2 * 32; len(a) != want {
		t.Errorf("nonce %q is %d chars, want %d (32 bytes hex-encoded)", a, len(a), want)
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("nonce %q is not hex: %v", a, err)
	}

	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	marker, ok, err := readWorkspaceHomeMarker(workspaceHomeMarkerPath(metaDir, "ws-a"))
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}
	if marker.HomeID != a {
		t.Errorf("marker.HomeID = %q, want the nonce written into the home %q — the two copies are what the identity check compares", marker.HomeID, a)
	}

	// Re-mint the same workspace: wipe the home so the next call re-inits.
	if err := os.RemoveAll(homeA); err != nil {
		t.Fatalf("wipe ws-a's home: %v", err)
	}
	_, a2 := read("ws-a")
	if a2 == a {
		t.Errorf("re-initializing ws-a produced the same nonce %q — the token must identify the incarnation of the home, not the workspace slug (a value derived from the slug, or any other fixed input, is not usable here)", a)
	}
}

// --- 17-b. nonce 読み取りの安全性 (codex review 2 巡目, Blocker) ---
//
// The nonce is the ONE file in this mechanism that a sandboxed job can write
// to, and the daemon reads it on every dispatch. So the read itself is part
// of the attack surface: before this hardening it was a bare os.ReadFile,
// which follows symlinks, happily opens a FIFO (blocking the daemon
// forever, wedging every later dispatch for every workspace that goes
// through the same code path) and grows its buffer to whatever the file
// yields (a symlink to /dev/zero reads until OOM). Both outcomes hit the
// daemon — the trusted side — so a single job could take out unrelated
// tasks, which is well outside the "worst case is a redundant re-init"
// contract the nonce is documented to have.

// readNonceWithin runs readWorkspaceHomeNonce(homeDir) in a goroutine and
// fails the test if it has not returned within d, instead of deadlocking.
// Required because the exact regression under test — a nonce replaced by a
// FIFO — makes an unguarded read block forever, which would turn a red run
// into `go test`'s 10-minute panic dump rather than a legible failure.
func readNonceWithin(t *testing.T, homeDir string, d time.Duration) (string, bool, error) {
	t.Helper()
	type result struct {
		value  string
		exists bool
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		v, ok, err := readWorkspaceHomeNonce(homeDir)
		ch <- result{v, ok, err}
	}()
	select {
	case r := <-ch:
		return r.value, r.exists, r.err
	case <-time.After(d):
		t.Fatalf("readWorkspaceHomeNonce(%q) did not return within %s — the daemon must never block on a file a sandboxed job controls", homeDir, d)
		return "", false, nil
	}
}

// resolveWorkspaceHomeWithin is readNonceWithin's whole-call equivalent, for
// the two end-to-end guards below: what actually matters is that the next
// DISPATCH is not wedged, not merely that one helper returns.
func resolveWorkspaceHomeWithin(t *testing.T, r *Runner, slug string, d time.Duration) (string, error) {
	t.Helper()
	type result struct {
		home string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		home, err := r.resolveWorkspaceHome(slug)
		ch <- result{home, err}
	}()
	select {
	case res := <-ch:
		return res.home, res.err
	case <-time.After(d):
		t.Fatalf("resolveWorkspaceHome(%q) did not return within %s — a job must not be able to wedge the next dispatch by tampering with its own $HOME", slug, d)
		return "", nil
	}
}

// elide shortens a value for a failure message. One of the rejections under
// test is "this file is megabytes long", and dumping the whole thing into
// the test log makes a red run unreadable.
func elide(s string) string {
	const max = 64
	if len(s) <= max {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q...(%d bytes total)", s[:max], len(s))
}

// TestReadWorkspaceHomeNonce_RejectsUnsafeFiles pins the read-side contract
// file-shape by file-shape: only a small, regular, non-symlinked file is
// accepted; everything else is reported as unreadable, which
// workspaceHomeInitialized turns into a re-init (the fail-safe direction).
func TestReadWorkspaceHomeNonce_RejectsUnsafeFiles(t *testing.T) {
	const goodNonce = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name       string
		prepare    func(t *testing.T, homeDir, noncePath string)
		wantValue  string
		wantExists bool
		wantErr    bool
	}{
		{
			name: "regular file is read, trailing newline trimmed",
			prepare: func(t *testing.T, _, noncePath string) {
				if err := os.WriteFile(noncePath, []byte(goodNonce+"\n"), 0o600); err != nil {
					t.Fatalf("write nonce: %v", err)
				}
			},
			wantValue:  goodNonce,
			wantExists: true,
		},
		{
			name:    "absent file is not an error",
			prepare: func(t *testing.T, _, _ string) {},
		},
		{
			name: "symlink is not followed",
			prepare: func(t *testing.T, _, noncePath string) {
				target := filepath.Join(t.TempDir(), "outside-the-home")
				if err := os.WriteFile(target, []byte(goodNonce), 0o600); err != nil {
					t.Fatalf("write symlink target: %v", err)
				}
				if err := os.Symlink(target, noncePath); err != nil {
					t.Fatalf("symlink nonce: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "fifo is refused instead of blocking",
			prepare: func(t *testing.T, _, noncePath string) {
				if err := syscall.Mkfifo(noncePath, 0o600); err != nil {
					t.Fatalf("mkfifo nonce: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "directory is refused",
			prepare: func(t *testing.T, _, noncePath string) {
				if err := os.Mkdir(noncePath, 0o700); err != nil {
					t.Fatalf("mkdir nonce: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "oversized regular file is refused instead of being slurped",
			prepare: func(t *testing.T, _, noncePath string) {
				big := make([]byte, 4<<20)
				for i := range big {
					big[i] = 'a'
				}
				if err := os.WriteFile(noncePath, big, 0o600); err != nil {
					t.Fatalf("write oversized nonce: %v", err)
				}
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			tc.prepare(t, homeDir, workspaceHomeNoncePath(homeDir))

			value, exists, err := readNonceWithin(t, homeDir, 10*time.Second)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("readWorkspaceHomeNonce = (%s, %v, nil), want a non-nil error so the caller re-inits", elide(value), exists)
				}
				if exists {
					t.Errorf("readWorkspaceHomeNonce reported exists=true alongside an error; a rejected file must not look like a usable identity")
				}
				if value != "" {
					t.Errorf("readWorkspaceHomeNonce returned value %s alongside an error, want \"\"", elide(value))
				}
				return
			}
			if err != nil {
				t.Fatalf("readWorkspaceHomeNonce: %v", err)
			}
			if exists != tc.wantExists {
				t.Errorf("exists = %v, want %v", exists, tc.wantExists)
			}
			if value != tc.wantValue {
				t.Errorf("value = %q, want %q", value, tc.wantValue)
			}
		})
	}
}

// TestResolveWorkspaceHome_NonceReplacedByFifo_DoesNotWedgeNextDispatch is
// the end-to-end statement of the same Blocker: a job that leaves a FIFO
// behind where its nonce used to be must cost one extra init.sh run, not the
// daemon's ability to dispatch anything at all. An unguarded os.ReadFile
// blocks in open(2) until some process opens the FIFO for writing — which,
// after the job's container is gone, never happens.
func TestResolveWorkspaceHome_NonceReplacedByFifo_DoesNotWedgeNextDispatch(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")
	r := splitRootRunner(t)

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	counter := filepath.Join(homeDir, "counter")
	if lines := countLines(t, counter); lines != 1 {
		t.Fatalf("counter lines after first call = %d, want 1", lines)
	}

	noncePath := workspaceHomeNoncePath(homeDir)
	if err := os.Remove(noncePath); err != nil {
		t.Fatalf("remove nonce: %v", err)
	}
	if err := syscall.Mkfifo(noncePath, 0o600); err != nil {
		t.Fatalf("mkfifo over the nonce path: %v", err)
	}

	if _, err := resolveWorkspaceHomeWithin(t, r, "myws", 10*time.Second); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if lines := countLines(t, counter); lines != 2 {
		t.Errorf("counter lines = %d, want 2 (an unreadable nonce must force exactly one re-init)", lines)
	}
	info, err := os.Lstat(noncePath)
	if err != nil {
		t.Fatalf("lstat nonce after re-init: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("nonce is still %s after re-init, want a regular file (the re-init must replace the tampered entry)", info.Mode())
	}

	// ...and the workspace settles again rather than re-initializing forever.
	if _, err := resolveWorkspaceHomeWithin(t, r, "myws", 10*time.Second); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if lines := countLines(t, counter); lines != 2 {
		t.Errorf("counter lines after third call = %d, want 2 (the recovery is one-shot)", lines)
	}
}

// TestResolveWorkspaceHome_NonceReplacedByDirectory_RecoversInOneReInit is
// the case the read-side hardening alone does not cover (codex review round 3,
// Major). Refusing to READ a directory is only half the recovery: the re-init
// it triggers then has to be able to PUT a regular file back at that path,
// and os.Rename onto an existing directory fails with EISDIR no matter what
// the source is. Without the write-side fix the workspace wedges — every
// subsequent dispatch re-runs init.sh, fails at the rename, and leaves the
// directory in place to do it all again — which breaks PR2's declared
// contract that the worst outcome of a tampered identity file is exactly one
// extra idempotent init run.
func TestResolveWorkspaceHome_NonceReplacedByDirectory_RecoversInOneReInit(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")
	r := splitRootRunner(t)

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	counter := filepath.Join(homeDir, "counter")
	if lines := countLines(t, counter); lines != 1 {
		t.Fatalf("counter lines after first call = %d, want 1", lines)
	}

	noncePath := workspaceHomeNoncePath(homeDir)
	if err := os.Remove(noncePath); err != nil {
		t.Fatalf("remove nonce: %v", err)
	}
	// Non-empty, so a plain os.Remove on the write side would not be enough
	// either — a sandboxed job is under no obligation to leave it empty.
	if err := os.MkdirAll(filepath.Join(noncePath, "decoy"), 0o700); err != nil {
		t.Fatalf("mkdir over the nonce path: %v", err)
	}

	if _, err := resolveWorkspaceHomeWithin(t, r, "myws", 10*time.Second); err != nil {
		t.Fatalf("second call: %v — a directory at the identity path must not fail the dispatch", err)
	}
	if lines := countLines(t, counter); lines != 2 {
		t.Errorf("counter lines = %d, want 2 (a directory at the nonce path must force exactly one re-init)", lines)
	}
	info, err := os.Lstat(noncePath)
	if err != nil {
		t.Fatalf("lstat nonce after re-init: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("nonce is still %s after re-init, want a regular file", info.Mode())
	}

	// ...and the workspace settles rather than re-initializing forever.
	if _, err := resolveWorkspaceHomeWithin(t, r, "myws", 10*time.Second); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if lines := countLines(t, counter); lines != 2 {
		t.Errorf("counter lines after third call = %d, want 2 (the recovery is one-shot)", lines)
	}
}

// TestResolveWorkspaceHome_NonceReplacedBySymlink_NotFollowed pins the
// no-symlink half where it is actually load-bearing rather than merely
// hygienic: the symlink here points at a file holding the CORRECT nonce
// value, so a read that follows it observes a match and SKIPS init. The
// value is unguessable from inside a sandbox, so this is not a practical
// forgery — it is the construction that makes "we do not follow the symlink"
// the only reason the test can pass, instead of "the tampered content
// happened not to match anyway", which is what the other tamper tests in
// this file already cover.
func TestResolveWorkspaceHome_NonceReplacedBySymlink_NotFollowed(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")
	r := splitRootRunner(t)

	homeDir, err := r.resolveWorkspaceHome("myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	counter := filepath.Join(homeDir, "counter")
	if lines := countLines(t, counter); lines != 1 {
		t.Fatalf("counter lines after first call = %d, want 1", lines)
	}

	noncePath := workspaceHomeNoncePath(homeDir)
	real, err := os.ReadFile(noncePath)
	if err != nil {
		t.Fatalf("read nonce: %v", err)
	}
	stash := filepath.Join(t.TempDir(), "stashed-nonce")
	if err := os.WriteFile(stash, real, 0o600); err != nil {
		t.Fatalf("stash nonce: %v", err)
	}
	if err := os.Remove(noncePath); err != nil {
		t.Fatalf("remove nonce: %v", err)
	}
	if err := os.Symlink(stash, noncePath); err != nil {
		t.Fatalf("symlink nonce: %v", err)
	}

	if _, err := resolveWorkspaceHomeWithin(t, r, "myws", 10*time.Second); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if lines := countLines(t, counter); lines != 2 {
		t.Errorf("counter lines = %d, want 2 (the identity file must be read as a file, never followed as a symlink — a followed symlink here yields the matching value and skips init)", lines)
	}
}

// --- 17. 旧レイアウト (PR2 以前) からの移行契約 ---

// TestResolveWorkspaceHome_LegacyMarkerBesideHomes_NotReadNotDeleted pins the
// upgrade behavior PR2 of docs/plans/workspace-home-volume-persistence.md
// committed to in prose but never tested: a marker (and lock) left in the OLD
// location by a pre-PR2 daemon is neither read nor deleted. Observably that
// means exactly one extra init.sh run on the first dispatch after the
// upgrade, then steady state again.
//
// The legacy marker planted here is deliberately a COMPLETE one — current
// script hash, plus a home_id matching a nonce file seeded into the home — so
// it would satisfy every skip condition the current code has if the current
// code were reading that path. Without that, this test would be satisfied by
// the nonce check alone and would stop saying anything about the location.
func TestResolveWorkspaceHome_LegacyMarkerBesideHomes_NotReadNotDeleted(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	script := "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n"
	writeInitScript(t, configDir, "myws", script)
	r := splitRootRunner(t)

	homesDir, err := r.workspaceHomesDir()
	if err != nil {
		t.Fatalf("workspaceHomesDir: %v", err)
	}
	homeDir := filepath.Join(homesDir, "myws")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	const legacyNonce = "legacy-nonce-value"
	if err := os.WriteFile(filepath.Join(homeDir, workspaceHomeNonceFileName), []byte(legacyNonce), 0o600); err != nil {
		t.Fatalf("seed nonce: %v", err)
	}
	legacyMarkerPath := workspaceHomeMarkerPath(homesDir, "myws")
	if err := writeWorkspaceHomeMarker(legacyMarkerPath, workspaceHomeMarker{
		ScriptSHA256: scriptSHA256Hex([]byte(script), true),
		HomeID:       legacyNonce,
		CompletedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}
	legacyLockPath := workspaceHomeLockPath(homesDir, "myws")
	if err := os.WriteFile(legacyLockPath, nil, 0o600); err != nil {
		t.Fatalf("write legacy lock: %v", err)
	}

	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	counter := filepath.Join(homeDir, "counter")
	if _, statErr := os.Stat(counter); statErr != nil {
		t.Fatalf("init.sh was skipped: %v — a marker in the pre-PR2 location must not be read", statErr)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Fatalf("counter lines after upgrade = %d, want 1 (exactly one re-run)", lines)
	}

	// Steady state: the second call reads the NEW marker and skips.
	if _, err := r.resolveWorkspaceHome("myws"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Errorf("counter lines after second call = %d, want 1 (the re-run is one-shot, not per-dispatch)", lines)
	}

	// The new marker landed in homes-meta/...
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	if _, ok, err := readWorkspaceHomeMarker(workspaceHomeMarkerPath(metaDir, "myws")); err != nil || !ok {
		t.Fatalf("no marker written under the meta dir %s: ok=%v err=%v", metaDir, ok, err)
	}

	// ...and the legacy files are still exactly where they were. PR2 chose
	// not to delete them (destroying data the daemon has stopped relying on
	// buys nothing, and PR6/PR7 revisit the homes/ layout wholesale).
	legacyMarker, ok, err := readWorkspaceHomeMarker(legacyMarkerPath)
	if err != nil || !ok {
		t.Fatalf("legacy marker was deleted or corrupted: ok=%v err=%v", ok, err)
	}
	if legacyMarker.HomeID != legacyNonce {
		t.Errorf("legacy marker was rewritten: HomeID = %q, want the planted %q", legacyMarker.HomeID, legacyNonce)
	}
	if _, statErr := os.Stat(legacyLockPath); statErr != nil {
		t.Errorf("legacy lock file was deleted: %v", statErr)
	}
}
