package orchestrator

import (
	"errors"
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
//   - boid-root-*   (mount ROOT dirs; skipped while any mount is active underneath)
//   - boid-<jobID>-runner-{spec,state}.json  (go-native runner artifacts)
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

	mounts, err := readSystemMountPoints()
	if err != nil {
		slog.Warn("gc sandbox tmp: read mountinfo failed; skipping boid-root-* cleanup", "error", err)
		mounts = nil
	}

	heldRoots, err := readChrootHolders()
	if err != nil {
		slog.Warn("gc sandbox tmp: scan /proc/*/root failed; skipping boid-root-* cleanup", "error", err)
		heldRoots = nil
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
			// Primary safety check: a live process is chrooted here.
			// `unshare --root=` plus chroot makes /proc/<pid>/root point at the
			// sandbox ROOT directory; deleting that directory while a sandbox
			// process holds it destroys every directory entry the sandbox can
			// see (the root inode survives, but lookups all fail).
			if heldRoots == nil {
				continue
			}
			if _, held := heldRoots[full]; held {
				slog.Info("gc sandbox tmp: skipping boid-root-* with active chroot holder", "path", full)
				continue
			}
			// Secondary safety check: an unmounted-but-still-mounted bind
			// remains in some namespace's mount table. Rare in practice but
			// cheap to guard against.
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

// readSystemMountPoints walks /proc/<pid>/mountinfo for every process the
// caller can read, deduplicating by mount-namespace inode, and returns the
// union of mountpoints across all reachable namespaces.
//
// Reading only /proc/self/mountinfo misses bind mounts that live inside other
// mount namespaces — notably the boid exec sandbox, which runs in its own
// namespace via `unshare --mount`. A GC that consults only the caller's
// namespace would mistake the sandbox ROOT for an inactive leak and remove it
// while the sandbox is still alive (concretely: `os.RemoveAll(/tmp/boid-root-XYZ)`
// destroys the directory entry; the sandbox bash retains the inode but loses
// every directory lookup).
//
// Returns an error only when no /proc/<pid>/mountinfo could be read at all
// (extreme failure); a partial read is treated as the available view and the
// function returns nil error so callers can still proceed safely.
func readSystemMountPoints() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	seenNS := make(map[string]struct{})
	seenMounts := make(map[string]struct{})
	anyReadable := false

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !isAllDigits(name) {
			continue
		}
		// Dedup by mount-namespace inode so we read mountinfo at most once per
		// distinct namespace. Processes whose ns/mnt link is unreadable
		// (different uid, just-exited) are skipped.
		nsLink, err := os.Readlink(filepath.Join("/proc", name, "ns", "mnt"))
		if err != nil {
			continue
		}
		if _, ok := seenNS[nsLink]; ok {
			continue
		}
		seenNS[nsLink] = struct{}{}
		data, err := os.ReadFile(filepath.Join("/proc", name, "mountinfo"))
		if err != nil {
			continue
		}
		anyReadable = true
		for line := range strings.SplitSeq(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}
			// mountinfo format: mountID parentID major:minor root mountpoint ...
			seenMounts[fields[4]] = struct{}{}
		}
	}

	if !anyReadable {
		return nil, errors.New("no readable /proc/<pid>/mountinfo")
	}

	out := make([]string, 0, len(seenMounts))
	for m := range seenMounts {
		out = append(out, m)
	}
	return out, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// readChrootHolders scans /proc/<pid>/root for every readable process and
// returns the set of paths that any process is currently chrooted into. The
// path matches the value of `readlink /proc/<pid>/root`; a process whose root
// is the host filesystem yields "/" and is not included.
//
// boid sandboxes call `unshare --root=$ROOT` after laying out the bind mounts,
// which makes the sandbox bash's `/proc/<pid>/root` point at the sandbox ROOT
// directory. Removing that directory entry while the sandbox is alive is what
// produced the original bug — the inode lives on but the directory listing
// becomes empty (`echo /*` returns the literal `/*`).
//
// A " (deleted)" suffix on the readlink (already-removed sandbox) is stripped
// so the caller can match against /tmp/boid-root-XXX. We still report it: if
// the directory has been re-created by a later sandbox we want to protect the
// new one, even though the older holder is doomed.
func readChrootHolders() (map[string]struct{}, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	held := make(map[string]struct{})
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !isAllDigits(name) {
			continue
		}
		link, err := os.Readlink(filepath.Join("/proc", name, "root"))
		if err != nil {
			continue
		}
		link = strings.TrimSuffix(link, " (deleted)")
		if link == "/" || link == "" {
			continue
		}
		held[link] = struct{}{}
	}
	return held, nil
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
