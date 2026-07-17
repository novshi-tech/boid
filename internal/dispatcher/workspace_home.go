package dispatcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// workspaceHomeMarker is the on-disk completion marker for a workspace home
// init run. It lives at homes/<slug>.init.json — deliberately outside the
// home directory itself (docs/plans/home-workspace-volume.md 「置き場」),
// so a sandboxed job that gets an rw bind of the home directory cannot forge
// or tamper with the record of what was run.
type workspaceHomeMarker struct {
	ScriptSHA256 string    `json:"script_sha256"`
	BoidVersion  string    `json:"boid_version"`
	CompletedAt  time.Time `json:"completed_at"`
}

// resolveWorkspaceHome ensures the on-disk home directory for the workspace
// identified by workspaceID exists and, if the workspace declares an
// init.sh, that it has been run for the current content of that script. It
// returns the absolute path to the (now-ready) workspace home directory.
//
// Phase 4 PR1 (docs/plans/home-workspace-volume.md): this is wiring only —
// the returned directory is threaded into SandboxRuntimeInfo.WorkspaceHomeDir
// but BuildSandboxSpec does not read it yet (PR2 switches the sandbox HOME
// mount over to it). Behavior is otherwise unchanged.
//
// Contract (see the plan doc's 契約 section):
//   - the home directory always exists on return (nil error)
//   - init.sh runs at most once per distinct script content: a completion
//     marker keyed by the script's sha256 short-circuits every later call
//     with the same content
//   - concurrent calls for the same slug serialize on a flock so the script
//     runs exactly once; waiters block until the winner finishes and then
//     re-check the marker
//   - a failing init script returns an error and leaves no marker, so the
//     next call retries from scratch
func (r *Runner) resolveWorkspaceHome(workspaceID string) (string, error) {
	slug, err := normalizeWorkspaceSlug(workspaceID)
	if err != nil {
		return "", err
	}

	homesDir, err := r.workspaceHomesDir()
	if err != nil {
		return "", fmt.Errorf("workspace home: %w", err)
	}
	if err := os.MkdirAll(homesDir, 0o700); err != nil {
		return "", fmt.Errorf("workspace home: create homes dir %q: %w", homesDir, err)
	}

	homeDir := filepath.Join(homesDir, slug)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return "", fmt.Errorf("workspace home %q: create home dir %q: %w", slug, homeDir, err)
	}

	scriptPath, err := workspaceInitScriptPath(slug)
	if err != nil {
		return "", fmt.Errorf("workspace home %q: %w", slug, err)
	}
	scriptBytes, scriptExists, err := readIfExists(scriptPath)
	if err != nil {
		return "", fmt.Errorf("workspace home %q: read init script %q: %w", slug, scriptPath, err)
	}
	scriptSHA := scriptSHA256Hex(scriptBytes, scriptExists)

	markerPath := workspaceHomeMarkerPath(homesDir, slug)

	// Fast path: already initialized for this exact script content, no lock
	// needed.
	if marker, ok, err := readWorkspaceHomeMarker(markerPath); err != nil {
		return "", fmt.Errorf("workspace home %q: read marker %q: %w", slug, markerPath, err)
	} else if ok && marker.ScriptSHA256 == scriptSHA {
		return homeDir, nil
	}

	lockPath := workspaceHomeLockPath(homesDir, slug)
	release, err := acquireWorkspaceHomeLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("workspace home %q: acquire init lock: %w", slug, err)
	}
	defer release()

	// Re-read + re-hash under the lock (TOCTOU fix, codex review PR #787):
	// the fast-path read above happened before we serialized with any
	// concurrent writer of scriptPath, so scriptBytes/scriptSHA could
	// already be stale by the time we get here. Re-reading now, with the
	// lock held, makes this the authoritative snapshot for both the
	// double-checked marker compare directly below and the bytes actually
	// executed further down — so the hash recorded in the marker can never
	// diverge from the content that ran.
	scriptBytes, scriptExists, err = readIfExists(scriptPath)
	if err != nil {
		return "", fmt.Errorf("workspace home %q: read init script %q: %w", slug, scriptPath, err)
	}
	scriptSHA = scriptSHA256Hex(scriptBytes, scriptExists)

	// Double-checked: another dispatch may have finished init while we were
	// waiting on the lock.
	if marker, ok, err := readWorkspaceHomeMarker(markerPath); err != nil {
		return "", fmt.Errorf("workspace home %q: read marker %q: %w", slug, markerPath, err)
	} else if ok && marker.ScriptSHA256 == scriptSHA {
		return homeDir, nil
	}

	if scriptExists {
		if err := runWorkspaceInitScript(scriptBytes, homesDir, slug, homeDir); err != nil {
			return "", fmt.Errorf("workspace home %q: init script failed: %w", slug, err)
		}
	}

	marker := workspaceHomeMarker{
		ScriptSHA256: scriptSHA,
		BoidVersion:  boidVersion,
		CompletedAt:  time.Now().UTC(),
	}
	if err := writeWorkspaceHomeMarker(markerPath, marker); err != nil {
		return "", fmt.Errorf("workspace home %q: write marker: %w", slug, err)
	}

	return homeDir, nil
}

// boidVersion is embedded into every completion marker's boid_version field.
// No build-time version stamping (ldflags -X) exists in this repo yet, so
// this stays empty for now — a later PR can wire it up without touching the
// marker format (see the plan doc's BoidVersion note).
const boidVersion = ""

// normalizeWorkspaceSlug maps a JobSpec/Project WorkspaceID to the slug used
// to key a workspace home directory. An empty WorkspaceID (a project not
// assigned to any explicit workspace) normalizes to the default workspace's
// slug, matching resolveWorkspaceProxy's treatment of the same field
// elsewhere in this package.
func normalizeWorkspaceSlug(workspaceID string) (string, error) {
	if workspaceID == "" {
		return orchestrator.DefaultWorkspaceSlug, nil
	}
	if err := orchestrator.ValidWorkspaceSlug(workspaceID); err != nil {
		return "", fmt.Errorf("workspace home: %w", err)
	}
	return workspaceID, nil
}

// workspaceDataHomeRoot returns ~/.local/share/boid (or
// $XDG_DATA_HOME/boid), matching the XDG_DATA_HOME-first convention used
// throughout this codebase (e.g. cmd/start.go's defaultDBPath).
func workspaceDataHomeRoot() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "boid"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("could not determine user home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "boid"), nil
}

// WorkspaceHomesDir returns ~/.local/share/boid/homes, the parent directory
// under which every workspace's home lives (docs/plans/home-workspace-volume.md
// 「レイアウト」). Unlike runtimes/, this directory is never GC'd — workspace
// homes are persistent (PR5 wires deletion to `workspace remove`).
//
// Prefers deriving from runtimesDir when non-empty: RuntimesDir is wired by
// server/wire.go as runtimesDirFor(cfg) — filepath.Dir(cfg.DBPath) (or
// cfg.SocketPath's dir when DBPath is ":memory:") + "/runtimes" — the same
// per-installation data root skills/ already lives under (see
// server.New's skillsDir). Deriving homes/ from the same root means a
// daemon instance running against a non-default DBPath (an isolated data
// dir, e.g. every test in this codebase that spins up a real server) gets
// its own isolated homes/ next to its own DB/runtimes, instead of every
// such instance converging on one global ~/.local/share/boid/homes and
// leaking into the real developer machine's home directory during `go
// test`. Falls back to the $XDG_DATA_HOME / ~/.local/share/boid convention
// only when runtimesDir is empty (minimal test wiring that constructs a
// bare &Runner{}, or a daemon build that never wired RuntimesDir).
//
// Exported (Phase 4 PR5, docs/plans/home-workspace-volume.md) as a pure
// free function — independent of any *Runner state — so internal/api's
// handlers (GET /api/workspaces/{slug} size reporting, POST /api/gc's
// workspace_homes listing, DELETE /api/workspaces/{slug}'s home dir
// deletion) can resolve the exact same homes/ directory the dispatcher
// itself uses, from the same runtimesDirFor(cfg) value server/wire.go
// already threads through those handlers, without needing a live *Runner.
func WorkspaceHomesDir(runtimesDir string) (string, error) {
	if runtimesDir != "" {
		return filepath.Join(filepath.Dir(runtimesDir), "homes"), nil
	}
	root, err := workspaceDataHomeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "homes"), nil
}

// workspaceHomesDir is a thin *Runner-bound wrapper around WorkspaceHomesDir.
func (r *Runner) workspaceHomesDir() (string, error) {
	return WorkspaceHomesDir(r.RuntimesDir)
}

// workspaceHomeMarkerPath returns homesDir/<slug>.init.json.
func workspaceHomeMarkerPath(homesDir, slug string) string {
	return filepath.Join(homesDir, slug+".init.json")
}

// workspaceHomeLockPath returns homesDir/<slug>.lock, the flock used to
// serialize concurrent init runs for the same slug.
func workspaceHomeLockPath(homesDir, slug string) string {
	return filepath.Join(homesDir, slug+".lock")
}

// workspaceInitScriptPath returns ~/.config/boid/workspaces/<slug>/init.sh
// (or $XDG_CONFIG_HOME's equivalent), mirroring
// orchestrator.DefaultWorkspaceDir's XDG_CONFIG_HOME-via-os.UserConfigDir
// convention. This is a plain host-config file, not a DB-backed workspace
// resource (docs/plans/home-workspace-volume.md 「置き場」決定: init.sh is
// environment-dependent shell content, outside the workspace's otherwise
// environment-independent DB-backed config, and stays in the same
// file-based category as host_commands.yaml).
func workspaceInitScriptPath(slug string) (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("could not determine user config directory: %w", err)
	}
	return filepath.Join(configDir, "boid", "workspaces", slug, "init.sh"), nil
}

// readIfExists reads path, returning (nil, false, nil) when it does not
// exist rather than an error.
func readIfExists(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// scriptSHA256Hex returns the hex-encoded sha256 of scriptBytes, or the
// empty string when exists is false — the documented "init not required"
// marker value (docs/plans/home-workspace-volume.md).
func scriptSHA256Hex(scriptBytes []byte, exists bool) string {
	if !exists {
		return ""
	}
	sum := sha256.Sum256(scriptBytes)
	return hex.EncodeToString(sum[:])
}

// readWorkspaceHomeMarker reads and parses the completion marker at path.
// Returns (zero, false, nil) when the file does not exist. A marker that
// exists but fails to parse (truncated write, manual edit) is treated the
// same as "does not exist" rather than a hard error — resolveWorkspaceHome
// then re-runs init and overwrites the corrupt marker with a fresh one,
// which is safe precisely because init scripts are contractually idempotent
// (docs/plans/home-workspace-volume.md 「script 作者が守ること」).
func readWorkspaceHomeMarker(path string) (workspaceHomeMarker, bool, error) {
	data, exists, err := readIfExists(path)
	if err != nil {
		return workspaceHomeMarker{}, false, err
	}
	if !exists {
		return workspaceHomeMarker{}, false, nil
	}
	var m workspaceHomeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Warn("workspace home: corrupt completion marker, treating as absent and re-initializing",
			"path", path, "error", err)
		return workspaceHomeMarker{}, false, nil
	}
	return m, true, nil
}

// writeWorkspaceHomeMarker writes marker to path via a sibling temp file +
// rename, so concurrent readers (or a crash mid-write) never observe a
// partially written marker.
func writeWorkspaceHomeMarker(path string, marker workspaceHomeMarker) (retErr error) {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".init.json.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp marker file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp marker file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp marker file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp marker file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp marker file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename marker file: %w", err)
	}
	cleanup = false
	// Best-effort parent dir fsync so the rename survives a crash right
	// after this call returns; not fatal if unsupported.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

// acquireWorkspaceHomeLock opens (creating if needed) lockPath and takes an
// exclusive advisory flock on it, returning a release function that MUST be
// called (typically via defer) to unlock and close the file. Mirrors
// internal/profiles/write.go's LockConfigMutation and
// internal/client/autostart.go's ensureRunning lock pattern.
func acquireWorkspaceHomeLock(lockPath string) (release func(), err error) {
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("flock %q: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// runWorkspaceInitScript executes scriptBytes on the host (trusted side,
// never inside the sandbox) with HOME redirected to homeDir and
// BOID_WORKSPACE_SLUG / BOID_WORKSPACE_HOME set, per the plan doc's
// contract. Combined stdout+stderr is logged on success and included (tail)
// in the returned error on failure.
//
// scriptBytes — not a path — is what gets executed: it is written to a
// private temp file inside homesDir (already lock-serialized and isolated
// from any sandbox mount) and run from there, rather than re-opening the
// configured init.sh path by name. Re-resolving that path at spawn time
// would reopen the exact TOCTOU window resolveWorkspaceHome's caller just
// closed by re-reading+re-hashing under the lock: a writer could otherwise
// swap the on-disk init.sh between the hash computation and the actual
// exec, so the marker would record a hash for content that never ran
// (codex review, PR #787). Executing the very bytes that were just hashed
// makes "hash recorded == content executed" true by construction.
//
// Trade-off: init.sh's shebang line is ignored (always run via /bin/bash,
// matching prior behavior) and $0 inside the script resolves to the temp
// path rather than the configured init.sh location — scripts must not
// depend on their own path (documented contract in the plan doc).
func runWorkspaceInitScript(scriptBytes []byte, homesDir, slug, homeDir string) error {
	tmpFile, err := os.CreateTemp(homesDir, "."+slug+".init.sh.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp init script: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmpFile.Write(scriptBytes); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp init script %q: %w", tmpPath, err)
	}
	if err := tmpFile.Chmod(0o700); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod temp init script %q: %w", tmpPath, err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync temp init script %q: %w", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp init script %q: %w", tmpPath, err)
	}

	cmd := exec.Command("/bin/bash", tmpPath)
	cmd.Dir = homeDir
	cmd.Env = buildWorkspaceInitEnv(slug, homeDir)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := cmd.Run()
	output := out.String()
	if runErr != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		const maxTail = 4096
		tail := output
		if len(tail) > maxTail {
			tail = tail[len(tail)-maxTail:]
		}
		return fmt.Errorf("init script (workspace %q) exited %d: %w\n--- output tail ---\n%s", slug, exitCode, runErr, tail)
	}

	slog.Info("workspace init script completed",
		"workspace_slug", slug, "output_bytes", len(output))
	return nil
}

// buildWorkspaceInitEnv builds the environment for an init.sh run: the
// contractually guaranteed vars (HOME, BOID_WORKSPACE_SLUG,
// BOID_WORKSPACE_HOME) plus a curated allowlist of host env vars a
// well-behaved installer script is likely to need (PATH for locating
// installers/package managers it shells out to, basic locale/user identity).
// Everything else is intentionally NOT inherited: the script's whole job is
// to populate a *different* HOME than the one the daemon process itself
// runs under, so carrying over host XDG_*/HOME-relative vars unchanged would
// be actively misleading.
func buildWorkspaceInitEnv(slug, homeDir string) []string {
	env := []string{
		"HOME=" + homeDir,
		"BOID_WORKSPACE_SLUG=" + slug,
		"BOID_WORKSPACE_HOME=" + homeDir,
	}
	for _, key := range []string{"PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "TERM"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}
