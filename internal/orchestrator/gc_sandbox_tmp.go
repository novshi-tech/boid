package orchestrator

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scriptSuffixes are the sandbox temp-file name suffixes the dispatcher writes.
// The go-native runner writes -runner-spec.json (the JSON sandbox spec) and
// -runner-state.json (the diagnostic dump). The legacy bash suffixes
// (-inner.sh / -outer.sh / -setup.sh) are retained here only to sweep up any
// files leaked before the Phase 3-a cutover; the runner no longer produces them.
var scriptSuffixes = []string{
	"-runner-spec.json",
	"-runner-state.json",
	"-inner.sh", "-outer.sh", "-setup.sh",
}

// cleanSandboxTmp removes leaked sandbox temp artifacts from tmpDir:
//   - boid-root-*   (mount ROOT dirs)
//   - boid-<jobID>-runner-{spec,state}.json  (go-native runner artifacts)
//
// Only entries whose mtime is older than olderThan are considered. olderThan<=0
// disables the age filter. Returns the number of entries successfully removed.
//
// This is a safety net: under normal operation the dispatcher cleans these up
// synchronously after each sandbox run. Files only accumulate here when the
// daemon was killed (SIGKILL/OOM) or the host rebooted mid-run.
//
// Safe by construction: sandbox bind mounts live exclusively inside the
// pivot_root + MNT_DETACH + MS_PRIVATE mount namespace, so they are never
// visible from the host mount namespace this GC runs in — `os.RemoveAll` here
// cannot traverse onto host source data. A still-running sandbox keeps its
// pivot_root'd tmpfs alive via inode references; removing the host-side ROOT
// directory entry merely makes it unreachable from outside, which has no
// effect on the running sandbox (verified 2026-06-18, see
// [[stale-bind-mount-deletion-incident]] in memory). The earlier chroot-holder
// / system-mountinfo guards from the chroot-based runner are not needed here.
func cleanSandboxTmp(tmpDir string, olderThan time.Duration) int {
	if tmpDir == "" {
		return 0
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		slog.Warn("gc sandbox tmp: read dir failed", "dir", tmpDir, "error", err)
		return 0
	}

	cutoff := time.Time{}
	if olderThan > 0 {
		cutoff = time.Now().Add(-olderThan)
	}

	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		full := filepath.Join(tmpDir, name)

		if !isSandboxTmpCandidate(name, entry.IsDir()) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		if olderThan > 0 && info.ModTime().After(cutoff) {
			continue
		}

		if err := os.RemoveAll(full); err != nil {
			slog.Warn("gc sandbox tmp: remove failed", "path", full, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("gc sandbox tmp removed", "count", removed)
	}
	return removed
}

// isSandboxTmpCandidate reports whether the given name matches one of the
// sandbox temp artifact patterns.
func isSandboxTmpCandidate(name string, isDir bool) bool {
	if isDir {
		return strings.HasPrefix(name, "boid-root-")
	}
	if !strings.HasPrefix(name, "boid-") {
		return false
	}
	for _, s := range scriptSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}
