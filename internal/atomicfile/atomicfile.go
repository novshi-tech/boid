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
// silently replace it) — so "path exists" only ever means "someone else
// got there first", never a half-written file. A losing caller re-reads
// the winner's file instead of returning its own, never-actually-persisted
// content.
//
// Contract (docs/plans/volume-only-daemon.md §論点 d, fix for [Major 1,
// PR829 round 1 codex review]): this function is publish-if-absent + a
// safe read-back ONLY. There is no repair path. Earlier revisions renamed
// a temp file over a pre-existing EMPTY destination on the theory that an
// empty file has no live claimant — but that repair is not itself
// one-winner atomic (two concurrent callers can each observe the same
// empty file and each Rename over it, "last write wins" rather than a
// single winner), and it silently discarded any error from the read-back
// (including EACCES / other transient I/O errors) and fell through to an
// unconditional Rename that could clobber a file it never actually
// verified. Both hazards are closed by removing the repair branch
// entirely: an empty existing file, or any read-back error, is now
// reported to the caller rather than acted on. Every current caller
// (internal/mtls.LoadOrCreate, internal/dispatcher.LoadOrCreateKey) treats
// the files this function publishes as volatile/regenerable (the plan
// doc's own framing), so "fail and tell the operator to remove the stale
// artifact manually" is an acceptable, safe default — silently guessing
// which of two racing writers should win is not.
//
// Durability note (Major 2, PR829 round 1 codex review, deliberately NOT
// fixed here — comment-only per the follow-up scope): this function makes
// no crash-durability guarantee. Write+Close+Link can all report success
// while content still only lives in page cache; a crash or a delayed-
// allocation ENOSPC surfaced during writeback after this function returns
// can still leave path zero-length or short on the next boot. Closing that
// gap would need an fsync of the temp file before Link and an fsync of dir
// after — intentionally out of scope for this fix.
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
		// concurrent (or earlier) PublishIfAbsent call published it
		// first, its content is already complete — read it back and
		// return it as the winner. Any read failure (including EACCES —
		// we cannot verify the file, so we must not act on it) or an
		// empty read-back (no repair path — see this function's own doc
		// comment) is reported to the caller instead of silently
		// overwriting.
		existing, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("atomicfile: read existing %s: %w", path, rerr)
		}
		if len(existing) == 0 {
			return nil, fmt.Errorf("atomicfile: %s exists but is empty (stale artifact — remove it manually)", path)
		}
		return existing, nil
	}
	return content, nil
}
