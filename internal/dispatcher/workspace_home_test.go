package dispatcher

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/integrationpack"
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
	// Wire does not set Backend (server/wire.go assigns it right after), and
	// resolveWorkspaceHome needs one to run the init container.
	r.Backend = newBashWorkspaceInitBackend(t)
	if r.DataHomeDir != dataHome {
		t.Fatalf("Wire dropped DataHomeDir: got %q, want %q", r.DataHomeDir, dataHome)
	}

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	metaDir := filepath.Join(dataHome, "homes-meta")
	for _, name := range []string{"myws.init.json", "myws.lock"} {
		if _, statErr := os.Stat(filepath.Join(metaDir, name)); statErr != nil {
			t.Errorf("%s not found under the DataHomeDir-derived meta dir %s: %v", name, metaDir, statErr)
		}
	}

	// ...and nothing was left behind under the (now unused) homes/ root. PR6
	// moved the home itself into a named volume, so that root should not be
	// touched at all — a daemon still mkdir'ing it there would be the pre-PR6
	// tmpfs layout quietly surviving the cutover.
	homesRoot := filepath.Join(filepath.Dir(runtimesDir), "homes")
	if _, statErr := os.Stat(homesRoot); !os.IsNotExist(statErr) {
		t.Errorf("the RuntimesDir-derived homes root %s exists; PR6 neither creates nor uses it", homesRoot)
	}
}

// --- 1. script 無し workspace の素通し ---

func TestResolveWorkspaceHome_NoScript_PassesThrough(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	if want := dockerres.WorkspaceHomeVolumeName(r.InstallID, "myws"); vol != want {
		t.Errorf("workspace home = %q, want the named volume %q", vol, want)
	}
	if info, statErr := os.Stat(be.dirFor(vol)); statErr != nil || !info.IsDir() {
		t.Fatalf("home volume not created: stat err=%v", statErr)
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

	r := newWorkspaceHomeTestRunner(t)
	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	homeDir := workspaceHomeDirOf(t, r, vol)

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

	r := newWorkspaceHomeTestRunner(t)
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if lines := countLines(t, filepath.Join(workspaceHomeDirOf(t, r, vol), "counter")); lines != 1 {
		t.Errorf("counter lines = %d, want 1 (script must not re-run for an unchanged script)", lines)
	}
}

// --- 4. script 内容変更で再実行 ---

func TestResolveWorkspaceHome_ScriptContentChanged_ReRuns(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")

	r := newWorkspaceHomeTestRunner(t)
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Same observable effect, different content -> different hash.
	writeInitScript(t, configDir, "myws", "#!/bin/bash\n# v2\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")

	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if lines := countLines(t, filepath.Join(workspaceHomeDirOf(t, r, vol), "counter")); lines != 2 {
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

	r := newWorkspaceHomeTestRunner(t)
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	homes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			home, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
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
			t.Errorf("goroutine %d home volume = %q, want %q (same as goroutine 0)", i, homes[i], homes[0])
		}
	}
	if t.Failed() {
		return
	}

	if lines := countLines(t, filepath.Join(workspaceHomeDirOf(t, r, homes[0]), "counter")); lines != 1 {
		t.Errorf("counter lines = %d, want exactly 1 across %d concurrent calls", lines, n)
	}
}

// --- 6. script 失敗で明示エラー ---

func TestResolveWorkspaceHome_ScriptFails_ReturnsErrorNoMarkerThenRetries(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho boom >&2\nexit 1\n")

	r := newWorkspaceHomeTestRunner(t)
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err == nil {
		t.Fatal("expected an error from a failing init script")
	}

	markerPath := filepath.Join(dataDir, "boid", "homes-meta", "myws.init.json")
	if _, ok, err := readWorkspaceHomeMarker(markerPath); err != nil || ok {
		t.Fatalf("marker must not be written on init failure: ok=%v err=%v", ok, err)
	}

	// Still failing -> still an error on retry (no silent "already tried"
	// caching of the failure).
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err == nil {
		t.Fatal("expected an error on retry while the script still fails")
	}

	// Fix the script; the next call must succeed and write the marker.
	writeInitScript(t, configDir, "myws", "#!/bin/bash\nexit 0\n")
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("expected success after fixing the script: %v", err)
	}
	if _, ok, err := readWorkspaceHomeMarker(markerPath); err != nil || !ok {
		t.Fatalf("marker should be written after a successful retry: ok=%v err=%v", ok, err)
	}
}

// --- 7. workspace slug 正規化 ("" -> default) ---

func TestResolveWorkspaceHome_EmptyWorkspaceID_UsesDefaultSlug(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := newWorkspaceHomeTestRunner(t)

	vol, slug, _, err := r.resolveWorkspaceHome(context.Background(), "")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if slug != orchestrator.DefaultWorkspaceSlug {
		t.Errorf("slug = %q, want %q", slug, orchestrator.DefaultWorkspaceSlug)
	}
	want := dockerres.WorkspaceHomeVolumeName(r.InstallID, orchestrator.DefaultWorkspaceSlug)
	if vol != want {
		t.Errorf("home volume = %q, want %q", vol, want)
	}
}

// --- 8. slug 検証 ---

func TestResolveWorkspaceHome_InvalidSlug_ReturnsError(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r := newWorkspaceHomeTestRunner(t)

	for _, bad := range []string{"../etc", "a b", "Has/Slash", "UPPERCASE", strings.Repeat("x", 65)} {
		if _, _, _, err := r.resolveWorkspaceHome(context.Background(), bad); err == nil {
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

	r := newWorkspaceHomeTestRunner(t)
	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	homeDir := workspaceHomeDirOf(t, r, vol)
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

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
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

	r := newWorkspaceHomeTestRunner(t)
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
		home, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
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

	executed, err := os.ReadFile(filepath.Join(workspaceHomeDirOf(t, r, res.home), "executed"))
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

	r := newWorkspaceHomeTestRunner(t)
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	homes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			home, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
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

	raw, err := os.ReadFile(filepath.Join(workspaceHomeDirOf(t, r, homes[0]), "executed"))
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

// TestResolveWorkspaceHome_TOCTOU_NoHostSideCopyOfInitShSurvivesTheRun is the
// PR5 successor of TestResolveWorkspaceHome_TOCTOU_TempInitScriptRemovedAfterRun.
//
// That test proved two things about the pre-PR5 host exec: execution went
// through a private temp copy rather than by re-opening the configured
// init.sh by name (the PR #787 TOCTOU fix), and the copy did not outlive the
// call. Both claims still matter; only the location moved. The bytes now
// reach the init container on its stdin — a file the daemon writes is not a
// path the sibling engine could mount anyway (build/container/compose.yml's
// KNOWN GAP) — and the copy the script actually runs is created INSIDE the
// container, under /tmp, by the wrapper's own heredoc.
//
// So the three assertions are: $0 is not the configured path (or the run
// would be re-opening it by name), $0 is not anywhere on the daemon's side of
// the boundary, and nothing was left behind — neither the container-local copy
// nor any stray file in the daemon's own homes-meta directory, which after
// PR5 holds exactly the marker and the lock.
func TestResolveWorkspaceHome_TOCTOU_NoHostSideCopyOfInitShSurvivesTheRun(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	scriptPath := writeInitScript(t, configDir, "myws",
		"#!/bin/bash\nprintf '%s\\n' \"$0\" > \"$BOID_WORKSPACE_HOME/executed-from\"\n")

	r := newWorkspaceHomeTestRunner(t)
	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}

	pathBytes, err := os.ReadFile(filepath.Join(workspaceHomeDirOf(t, r, vol), "executed-from"))
	if err != nil {
		t.Fatalf("read the path the script reported for itself: %v", err)
	}
	ranFrom := strings.TrimSpace(string(pathBytes))
	if ranFrom == "" {
		t.Fatal("script did not report its own $0")
	}
	if ranFrom == scriptPath {
		t.Fatalf("init.sh ran as %s — the configured path itself, not a private copy of the hashed bytes (TOCTOU fix, PR #787)", ranFrom)
	}

	metaDir := filepath.Join(dataDir, "boid", "homes-meta")
	if strings.HasPrefix(ranFrom, metaDir+string(filepath.Separator)) {
		t.Errorf("init.sh ran from %s, inside the daemon's own meta dir — as of PR5 the executed copy is created inside the init container, not by the daemon", ranFrom)
	}
	if _, statErr := os.Stat(ranFrom); !os.IsNotExist(statErr) {
		t.Errorf("the executed copy %s still exists after the run (stat err=%v), want ENOENT — the wrapper removes it", ranFrom, statErr)
	}

	entries, err := os.ReadDir(metaDir)
	if err != nil {
		t.Fatalf("read the meta dir: %v", err)
	}
	want := map[string]bool{"myws.init.json": true, "myws.lock": true}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("unexpected leftover %q in %s; after PR5 this directory holds only the completion marker and the lock", e.Name(), metaDir)
		}
	}
}

// TestResolveWorkspaceHome_InitRunsInAContainerNotOnTheDaemon pins the
// headline behavior change, from the side an operator would notice: the
// daemon hands the whole run — prep and init.sh alike — to its backend, once,
// including for a workspace that declares no init.sh at all (§D1 / 論点 b-2's
// reason for making prep unconditional).
func TestResolveWorkspaceHome_InitRunsInAContainerNotOnTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"with init.sh", "#!/bin/bash\ntrue\n"},
		{"pass-through workspace", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, configDir := setupWorkspaceHomeTestDirs(t)
			if tc.script != "" {
				writeInitScript(t, configDir, "myws", tc.script)
			}
			r, be := newWorkspaceHomeTestRunnerWithBackend(t)

			vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
			if err != nil {
				t.Fatalf("resolveWorkspaceHome: %v", err)
			}
			if got := be.runCount(); got != 1 {
				t.Fatalf("the backend ran %d inits, want exactly 1 (one container covers prep + init.sh)", got)
			}
			req := be.lastRequest(t)
			if req.Slug != "myws" {
				t.Errorf("request slug = %q, want %q", req.Slug, "myws")
			}
			if req.HomeSource != vol {
				t.Errorf("request home source = %q, want the resolved home volume %q", req.HomeSource, vol)
			}
			if req.HomeTarget != hostHomeDir() {
				t.Errorf("request home target = %q, want %q (the path a job sees its $HOME at)", req.HomeTarget, hostHomeDir())
			}
			if req.HomeID == "" {
				t.Error("request carries no home volume identity")
			}
			if !equalStringSets(req.SkeletonDirs, workspaceHomeSkeletonDirs()) {
				t.Errorf("request SkeletonDirs = %v, want %v", req.SkeletonDirs, workspaceHomeSkeletonDirs())
			}
			// The prep half landed regardless of whether there was a script.
			for _, rel := range workspaceHomeSkeletonDirs() {
				if _, statErr := os.Stat(filepath.Join(be.dirFor(vol), rel)); statErr != nil {
					t.Errorf("skeleton dir %q missing after init: %v", rel, statErr)
				}
			}
		})
	}
}

// TestResolveWorkspaceHome_InitEnvIsTheContainerContract is the
// resolveWorkspaceHome-level statement of §D9, and the reason it is here as
// well as in workspace_init_test.go: the pre-PR5 version of this assertion
// (TestResolveWorkspaceHome_FirstRun_ExecutesScriptWithExpectedEnv) read an
// `env` dump the script itself wrote, which under the in-process stand-in
// would also show whatever the TEST process happens to export. Asserting on
// the request boid actually built is the only way to state "nothing is
// inherited" from this level.
func TestResolveWorkspaceHome_InitEnvIsTheContainerContract(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\ntrue\n")
	// PATH is deliberately not clobbered here: the in-process stand-in has to
	// keep being able to find mkdir. Its absence from the request is covered
	// by the exhaustive key check below either way, and
	// TestBuildWorkspaceInitEnv_IsAContainerEnvironmentNotTheDaemonsOwn — which
	// runs no shell — clobbers it too.
	for _, key := range []string{"USER", "LOGNAME", "LANG", "LC_ALL", "TERM"} {
		t.Setenv(key, "host-value-for-"+key)
	}
	be := newBashWorkspaceInitBackend(t)
	r := &Runner{Backend: be}

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	got := be.lastRequest(t).Env
	want := map[string]string{
		"HOME":                hostHomeDir(),
		"BOID_WORKSPACE_SLUG": "myws",
		"BOID_WORKSPACE_HOME": hostHomeDir(),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("init env %s = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("init env carries %q = %q; the daemon process's own environment is not inherited (§D9)", k, got[k])
		}
	}
}

// --- 12-16. marker と実体の identity 突合 ---
//
// docs/plans/workspace-home-volume-persistence.md 論点b. PR2 moved the
// completion marker into the daemon's own persistent data root while the
// workspace home lived somewhere else entirely — first a tmpfs-backed
// directory, and as of PR6 a docker named volume. Those are two independent
// lifetimes, so "the marker says initialized" does not imply "the home it
// describes still exists". A random identity minted per home incarnation and
// recorded in the marker is what re-ties them together; PR6 moved it from a
// file inside the home onto the volume's own label, because the daemon can no
// longer read anything inside the home (workspaceHomeMarker.HomeID).
//
// The volume-specific cases — a deleted-and-recreated volume, an unlabelled
// one, the identity's format and per-incarnation freshness — live in
// workspace_home_volume_test.go.

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
		Backend:     newBashWorkspaceInitBackend(t),
	}
}

// TestResolveWorkspaceHome_HomeWipedWhileMarkerSurvives_ReRunsInit is the
// regression guard for the failure mode 論点b predicts: the workspace home
// vanishes (a stray `docker volume rm`, a reap misfire, a half-completed
// workspace remove) while the marker in the daemon's own persistent root
// survives, so the next dispatch skips init.sh and hands the job an EMPTY
// $HOME. The error that surfaces from there is the adapter's "CLI not found"
// (internal/adapters/claude/run.go), which points at an unconfigured init.sh
// rather than at the real cause.
//
// This is the end-to-end statement over the SPLIT ROOTS specifically — the
// marker's root and the home's are unrelated here, so the wipe cannot touch
// the marker by accident. workspace_home_volume_test.go states the same
// property from the volume's side.
func TestResolveWorkspaceHome_HomeWipedWhileMarkerSurvives_ReRunsInit(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n")
	r := splitRootRunner(t)

	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	be := r.Backend.(*bashWorkspaceInitBackend)
	if lines := countLines(t, filepath.Join(be.dirFor(vol), "counter")); lines != 1 {
		t.Fatalf("counter lines after first call = %d, want 1", lines)
	}

	// The home volume goes; the marker's root is untouched by that.
	be.removeVolume(t, vol)
	markerPath := workspaceHomeMarkerPath(filepath.Join(r.DataHomeDir, "homes-meta"), "myws")
	if _, ok, err := readWorkspaceHomeMarker(markerPath); err != nil || !ok {
		t.Fatalf("test setup bug: the marker must have survived the wipe: ok=%v err=%v", ok, err)
	}

	vol2, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if vol2 != vol {
		t.Fatalf("home volume name changed across calls: %q -> %q", vol, vol2)
	}
	counter := filepath.Join(be.dirFor(vol2), "counter")
	if _, statErr := os.Stat(counter); statErr != nil {
		t.Fatalf("init.sh was skipped for a wiped home: %v — the surviving marker must not be trusted on its own (docs/plans/workspace-home-volume-persistence.md 論点b)", statErr)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Errorf("counter lines after re-init = %d, want 1 (exactly one re-run against the fresh home)", lines)
	}

	// ...and the re-init must be one-shot: a third call sees a volume whose
	// identity now matches the (rewritten) marker again and skips.
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Errorf("counter lines after third call = %d, want 1 (the identity check must re-settle, not re-run on every dispatch)", lines)
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
// The legacy marker planted here is deliberately as COMPLETE as that era could
// make it — current script hash, plus a home_id matching a nonce file seeded
// into the old home directory — so it would satisfy that era's every skip
// condition if the current code were reading that path. PR6 additionally moved
// the home itself out of this directory, so the test now pins two things at
// once: the old marker is not read, and the old home directory is not touched
// (PR8's migration CLI has to be able to read it).
func TestResolveWorkspaceHome_LegacyMarkerBesideHomes_NotReadNotDeleted(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	script := "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n"
	writeInitScript(t, configDir, "myws", script)
	r := splitRootRunner(t)
	be := r.Backend.(*bashWorkspaceInitBackend)

	homesDir, err := WorkspaceHomesDir(r.RuntimesDir)
	if err != nil {
		t.Fatalf("WorkspaceHomesDir: %v", err)
	}
	legacyHomeDir := filepath.Join(homesDir, "myws")
	if err := os.MkdirAll(legacyHomeDir, 0o700); err != nil {
		t.Fatalf("mkdir legacy home: %v", err)
	}
	const legacyNonce = "legacy-nonce-value"
	if err := os.WriteFile(filepath.Join(legacyHomeDir, ".boid-workspace-home-id"), []byte(legacyNonce), 0o600); err != nil {
		t.Fatalf("seed legacy nonce: %v", err)
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

	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	counter := filepath.Join(be.dirFor(vol), "counter")
	if _, statErr := os.Stat(counter); statErr != nil {
		t.Fatalf("init.sh was skipped: %v — a marker in the pre-PR2 location must not be read", statErr)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Fatalf("counter lines after upgrade = %d, want 1 (exactly one re-run)", lines)
	}

	// Steady state: the second call reads the NEW marker and skips.
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
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
	// ...including the pre-PR6 home directory itself, which PR8's migration
	// CLI is the one that gets to read it.
	if _, statErr := os.Stat(filepath.Join(legacyHomeDir, ".boid-workspace-home-id")); statErr != nil {
		t.Errorf("the pre-PR6 home directory was disturbed: %v", statErr)
	}
}

// --- 18. 実行環境の世代 (PR5 codex round 2, Major 1) ---

// seedInitializedWorkspaceHome plants the state a workspace that was
// initialized by an EARLIER build is in: a populated home carrying an identity
// file, and a marker in the current location whose script hash and home_id
// both match it. Everything except the generation agrees, so a build that
// skips on hash+nonce alone treats this workspace as fully prepared and never
// runs init again.
//
// It returns the home directory and the marker path so callers can vary the
// one field under test.
func seedInitializedWorkspaceHome(t *testing.T, r *Runner, slug, script string, marker workspaceHomeMarker) (homeDir, markerPath string) {
	t.Helper()
	be, ok := r.Backend.(*bashWorkspaceInitBackend)
	if !ok {
		t.Fatalf("runner backend is %T, not the modelled-volume test backend", r.Backend)
	}
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	// The volume is created through the backend, so it carries a real identity
	// label — the marker below records THAT value. Seeding a made-up identity
	// instead would make every one of these tests pass on the identity check
	// alone, and they would stop saying anything about the field under test.
	volume := dockerres.WorkspaceHomeVolumeName(r.InstallID, slug)
	homeID, err := be.EnsureWorkspaceHomeVolume(context.Background(), WorkspaceHomeVolumeRequest{
		Slug: slug, Name: volume, CandidateID: "seeded-home-identity",
	})
	if err != nil {
		t.Fatalf("seed home volume: %v", err)
	}
	homeDir = be.dirFor(volume)
	marker.ScriptSHA256 = scriptSHA256Hex([]byte(script), true)
	marker.HomeID = homeID
	if marker.SkeletonDirs == nil {
		marker.SkeletonDirs = workspaceHomeSkeletonDirs()
	}
	marker.CompletedAt = time.Now().UTC()
	markerPath = workspaceHomeMarkerPath(metaDir, slug)
	if err := writeWorkspaceHomeMarker(markerPath, marker); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return homeDir, markerPath
}

// TestResolveWorkspaceHome_MarkerFromAnOlderExecutionGeneration_ReInitializes
// is the second codex round's Major 1.
//
// PR5 did not change WHAT init.sh does, it changed WHERE it runs — from the
// daemon process (host `$HOME` = ~/.local/share/boid/homes/<slug>) to a
// throwaway container whose `$HOME` is the path a JOB sees. A toolchain
// installer bakes that path into wrapper scripts, shebangs and symlinks, so a
// home prepared under the old path holds artifacts that are wrong for every
// job dispatched after the upgrade.
//
// The skip condition as PR5 shipped it looks only at the script hash and the
// home identity, and a workspace initialized under PR2-PR4 matches both — so
// the new execution path would never run for it and the whole point of PR5
// would not reach any existing installation. The marker therefore has to
// record the generation of the environment its run happened in, and a marker
// that does not record THIS generation cannot vouch for the home.
func TestResolveWorkspaceHome_MarkerFromAnOlderExecutionGeneration_ReInitializes(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	script := "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n"
	writeInitScript(t, configDir, "myws", script)
	r := splitRootRunner(t)

	// A PR2-PR4 marker: complete by that build's standards, no generation
	// field at all (the zero value here is what unmarshalling one produces).
	homeDir, _ := seedInitializedWorkspaceHome(t, r, "myws", script, workspaceHomeMarker{})

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	counter := filepath.Join(homeDir, "counter")
	if _, statErr := os.Stat(counter); statErr != nil {
		t.Fatalf("init was skipped for a home prepared by an older execution environment: %v — the absolute paths a toolchain baked into that home are still the daemon's old host paths", statErr)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Fatalf("counter lines = %d, want 1 (exactly one re-init)", lines)
	}

	// One-shot: the rewritten marker records the current generation, so the
	// next dispatch skips. A re-init on EVERY dispatch would be a far worse
	// regression than the one this fixes.
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Errorf("counter lines after the second call = %d, want 1 (the generation re-init must settle, not repeat)", lines)
	}
}

// TestResolveWorkspaceHome_MarkerRecordsTheCurrentExecutionGeneration pins the
// write side, and with it the contract PR8 depends on.
//
// PR8 copies an existing host home's CONTENTS into the named volume PR6
// introduces. If it carries the identity file across, the nonce check passes
// on the other side — so the generation stamped here is the only thing that
// still says "this home was prepared somewhere else". A marker that recorded
// nothing, or that recorded a constant nobody ever bumps, would let PR8 hand
// over a volume full of host-era absolute paths and report success.
func TestResolveWorkspaceHome_MarkerRecordsTheCurrentExecutionGeneration(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\nexit 0\n")
	r := splitRootRunner(t)

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	metaDir, err := r.workspaceHomeMetaDir()
	if err != nil {
		t.Fatalf("workspaceHomeMetaDir: %v", err)
	}
	marker, ok, err := readWorkspaceHomeMarker(workspaceHomeMarkerPath(metaDir, "myws"))
	if err != nil || !ok {
		t.Fatalf("no marker written: ok=%v err=%v", ok, err)
	}
	if marker.InitGeneration != workspaceHomeInitGeneration {
		t.Errorf("marker.InitGeneration = %d, want %d — a marker that does not record the environment its run happened in cannot be checked against a later one",
			marker.InitGeneration, workspaceHomeInitGeneration)
	}
}

// --- Integration Pack skill discoverability (shadow-b follow-up,
// docs/plans/signal-driven-review.md §6.4) ---

// TestResolveWorkspaceHome_CreatesPackSkillSymlinks pins the end-to-end path:
// a Pack loaded into Runner.Packs reaches the workspace home as a working
// .claude/skills/<name> symlink, readable through to the Pack's own content.
func TestResolveWorkspaceHome_CreatesPackSkillSymlinks(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	packDir := t.TempDir()
	skillDir := filepath.Join(packDir, "skills", "jira-api")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("jira api reference"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Packs = []*integrationpack.Pack{{
		Name: "jira-cloud", Version: "1.0.0", Dir: packDir,
		Manifest: integrationpack.Manifest{
			Skills: []integrationpack.Skill{{Name: "jira-api", Path: filepath.Join("skills", "jira-api")}},
		},
	}}

	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	homeDir := be.dirFor(vol)
	linkPath := filepath.Join(homeDir, ".claude", "skills", "jira-api")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("stat symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".claude/skills/jira-api is not a symlink (mode %s)", info.Mode())
	}
	data, err := os.ReadFile(filepath.Join(linkPath, "SKILL.md"))
	if err != nil {
		t.Fatalf("read through symlink: %v", err)
	}
	if string(data) != "jira api reference" {
		t.Errorf("content through symlink = %q, want %q", data, "jira api reference")
	}
}

// TestResolveWorkspaceHome_PackSkillSetChanged_ReInitializes pins the marker
// side: a workspace already settled on one Pack skill set must re-init when
// Runner.Packs changes (a Pack upgraded, added, or removed) — mirroring
// TestResolveWorkspaceHome_MarkerFromAnOlderExecutionGeneration_ReInitializes
// for PackSkillLinks instead of InitGeneration. Left unnoticed, a workspace
// initialized before a Pack was added would never gain that Pack's skill
// symlink until something else happened to force a re-init.
func TestResolveWorkspaceHome_PackSkillSetChanged_ReInitializes(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("first call (no Packs): %v", err)
	}

	packDir := t.TempDir()
	skillDir := filepath.Join(packDir, "skills", "slack-api")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.Packs = []*integrationpack.Pack{{
		Name: "slack", Version: "1.0.0", Dir: packDir,
		Manifest: integrationpack.Manifest{
			Skills: []integrationpack.Skill{{Name: "slack-api", Path: filepath.Join("skills", "slack-api")}},
		},
	}}

	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("second call (Pack added): %v", err)
	}
	linkPath := filepath.Join(be.dirFor(vol), ".claude", "skills", "slack-api")
	if info, err := os.Lstat(linkPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".claude/skills/slack-api symlink missing after Runner.Packs changed (a home initialized before this Pack existed must re-init): stat err=%v", err)
	}

	// One-shot: settles on the second marker, no re-init on a third call with
	// the same Packs.
	runsBefore := be.runCount()
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if be.runCount() != runsBefore {
		t.Errorf("resolveWorkspaceHome re-ran init on an unchanged Pack set (runs %d -> %d)", runsBefore, be.runCount())
	}
}

// TestResolveWorkspaceHome_PackRemoved_ReInitializesButLeavesStaleSymlink
// pins the OTHER direction of set-change detection, and — deliberately — the
// documented gap alongside it (workspaceHomeMarker.PackSkillLinks' own doc
// comment): removing a Pack is detected (a re-init happens), but the
// prelude only ever CREATES the current set's symlinks, so the previous
// run's now-orphaned .claude/skills/<name> is left behind as a dangling
// symlink rather than being cleaned up. If a future change adds a sweep
// step, this test's second assertion should start failing and can be
// flipped — that is the point of pinning the gap rather than leaving it
// undocumented.
func TestResolveWorkspaceHome_PackRemoved_ReInitializesButLeavesStaleSymlink(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	r, be := newWorkspaceHomeTestRunnerWithBackend(t)

	packDir := t.TempDir()
	skillDir := filepath.Join(packDir, "skills", "jira-api")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.Packs = []*integrationpack.Pack{{
		Name: "jira-cloud", Version: "1.0.0", Dir: packDir,
		Manifest: integrationpack.Manifest{
			Skills: []integrationpack.Skill{{Name: "jira-api", Path: filepath.Join("skills", "jira-api")}},
		},
	}}
	vol, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws")
	if err != nil {
		t.Fatalf("first call (Pack present): %v", err)
	}
	linkPath := filepath.Join(be.dirFor(vol), ".claude", "skills", "jira-api")
	if info, err := os.Lstat(linkPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("setup: symlink not created: err=%v", err)
	}

	runsBefore := be.runCount()
	// Removing packDir itself (not just clearing r.Packs) is what turns
	// this into an actually-DANGLING symlink rather than one that merely
	// stopped being listed — a review round (Opus round 2) found the
	// original version of this test only proved the link entry survived,
	// never that it stopped resolving to anything.
	if err := os.RemoveAll(packDir); err != nil {
		t.Fatal(err)
	}
	r.Packs = nil
	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("second call (Pack removed): %v", err)
	}
	if be.runCount() == runsBefore {
		t.Fatal("removing a Pack did not trigger a re-init (the marker's set comparison should have detected it)")
	}

	// The documented gap: the symlink ENTRY is not swept, so Lstat still
	// finds it...
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("stale symlink vanished on its own — the doc comment describing this as a known gap is now wrong: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected a dangling symlink at %q, got mode %s", linkPath, info.Mode())
	}
	// ...but it is genuinely DANGLING: the target packDir is gone, so
	// following the link (Stat, not Lstat) fails. Claude Code's own skill
	// discovery would find this entry and fail to read a SKILL.md through
	// it.
	if _, err := os.Stat(linkPath); err == nil {
		t.Fatalf("expected %q to be a dangling symlink (target removed), but it still resolves", linkPath)
	}
}

// TestResolveWorkspaceHome_MarkerFromANewerExecutionGeneration_ReInitializes
// keeps the comparison an EQUALITY rather than a floor.
//
// A `>=` test reads naturally ("at least as new as what we need") and is wrong
// in the one direction that actually happens: an operator rolls a boid release
// back after it bumped the generation, and every home is then a home the
// running build never prepared. Equality re-inits; a floor would skip.
func TestResolveWorkspaceHome_MarkerFromANewerExecutionGeneration_ReInitializes(t *testing.T) {
	_, configDir := setupWorkspaceHomeTestDirs(t)
	script := "#!/bin/bash\necho x >> \"$BOID_WORKSPACE_HOME/counter\"\n"
	writeInitScript(t, configDir, "myws", script)
	r := splitRootRunner(t)

	homeDir, _ := seedInitializedWorkspaceHome(t, r, "myws", script, workspaceHomeMarker{
		InitGeneration: workspaceHomeInitGeneration + 1,
	})

	if _, _, _, err := r.resolveWorkspaceHome(context.Background(), "myws"); err != nil {
		t.Fatalf("resolveWorkspaceHome: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(homeDir, "counter")); statErr != nil {
		t.Fatalf("init was skipped for a home prepared by a NEWER build's execution environment: %v", statErr)
	}
}
