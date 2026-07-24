// Package atomicfile provides the write-temp + os.Link publish-if-absent
// primitive every on-first-boot "load or generate and persist" secret file
// in boid needs (docs/plans/volume-only-daemon.md §論点 d — secret
// material generated into a fresh, empty named volume at daemon boot).
//
// internal/install.LoadOrCreate (docs/plans/phase6-container-backend.md
// §PR6, Major 7 codex review) pioneered this exact protocol for
// install_id, specifically to fix a real race: two daemon instances
// starting at once against the same fresh data dir could previously each
// independently observe "file missing", generate their own value, and
// clobber each other's plain os.WriteFile — with a window where a reader
// could see a half-written file in between. internal/dispatcher.
// LoadOrCreateKey (secret.key / web_secret) and internal/mtls.LoadOrCreate
// (the daemon's internal CA) both predate that fix and still use a plain
// os.WriteFile for their own generate-and-persist step. This package
// extracts the general primitive so all three (not just install_id) get
// the same guarantee, per volume-only-daemon.md's explicit instruction to
// reuse the existing pattern rather than invent a new one.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// PublishIfAbsent atomically publishes content to path if path does not
// already hold legitimate content, returning the winning content — this
// call's own content if it won the publish race, or whatever a concurrent
// (or earlier) winner already published if it lost. path's parent
// directory must already exist.
//
// Protocol (identical to internal/install.LoadOrCreate's): content is
// written to a temp file created in the same directory as path (so the
// follow-up publish step stays on one filesystem, a hard-link
// requirement), chmod'd to perm, then published via os.Link — which fails
// with os.IsExist if path already exists (unlike os.Rename, which would
// silently replace it) — so "path exists" only ever means "path already
// has complete content", never a half-written file. A losing caller
// re-reads the winner's file instead of returning its own,
// never-actually-persisted content. If path exists but holds no
// legitimate content (empty — e.g. a stale artifact from a crash
// predating this protocol, not a live concurrent writer), the temp file
// is renamed over it instead — safe because nothing else holds a
// legitimate claim on that content.
func PublishIfAbsent(path string, perm os.FileMode, content []byte) ([]byte, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".atomicfile-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Always clean up the temp name: os.Link (the success path) leaves it
	// behind as a second, now-redundant hard link to the same inode;
	// os.Rename (the repair path below) already consumes it, so this
	// becomes a harmless no-op ENOENT in that case.
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("atomicfile: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("atomicfile: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("atomicfile: close temp file: %w", err)
	}

	if err := os.Link(tmpPath, path); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("atomicfile: publish %s: %w", path, err)
		}
		// path already exists. Every writer that ever reaches this point
		// uses this same write-temp-then-Link protocol, so if a
		// concurrent PublishIfAbsent call published it first, its
		// content is already complete — re-reading finds it. The only
		// other way path can exist here with no legitimate content is a
		// stale artifact with no live writer racing us — os.Rename
		// (unlike another os.Link) replaces it outright rather than
		// failing, which is correct precisely because nothing else
		// holds a legitimate claim on it.
		if existing, rerr := os.ReadFile(path); rerr == nil && len(existing) > 0 {
			return existing, nil
		}
		if rerr := os.Rename(tmpPath, path); rerr != nil {
			return nil, fmt.Errorf("atomicfile: repair %s: %w", path, rerr)
		}
	}
	return content, nil
}
