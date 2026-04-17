package orchestrator

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scriptSuffixes are the sandbox script name suffixes written by
// sandbox.WriteSandboxScripts.
var scriptSuffixes = []string{"-inner.sh", "-outer.sh", "-setup.sh"}

// cleanSandboxTmp removes leaked sandbox temp artifacts from tmpDir:
//   - boid-root-*   (mount ROOT dirs; skipped while any mount is active underneath)
//   - boid-gates-*  (gate script staging dirs)
//   - boid-<jobID>-{inner,outer,setup}.sh  (generated sandbox scripts)
//
// Only entries whose mtime is older than olderThan are considered. olderThan<=0
// disables the age filter. Returns the number of entries successfully removed.
//
// This is a safety net: under normal operation the dispatcher cleans these up
// synchronously after each sandbox run. Files only accumulate here when the
// daemon was killed (SIGKILL/OOM) or the host rebooted mid-run.
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

	mounts, err := readMountPoints()
	if err != nil {
		slog.Warn("gc sandbox tmp: read mountinfo failed; skipping boid-root-* cleanup", "error", err)
		mounts = nil
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

		if strings.HasPrefix(name, "boid-root-") {
			if mounts == nil {
				continue
			}
			if hasActiveMountUnder(full, mounts) {
				slog.Info("gc sandbox tmp: skipping boid-root-* with active mount", "path", full)
				continue
			}
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
		return strings.HasPrefix(name, "boid-root-") || strings.HasPrefix(name, "boid-gates-")
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

// readMountPoints returns the list of mountpoints currently active in this
// process's mount namespace. Parses /proc/self/mountinfo.
func readMountPoints() ([]string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	var mounts []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// mountinfo format: mountID parentID major:minor root mountpoint ...
		mounts = append(mounts, fields[4])
	}
	return mounts, nil
}

// hasActiveMountUnder reports whether path or any subpath of path is in mounts.
func hasActiveMountUnder(path string, mounts []string) bool {
	prefix := path + "/"
	for _, m := range mounts {
		if m == path || strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}
