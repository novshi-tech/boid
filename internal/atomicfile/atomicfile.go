// Package atomicfile provides the write-temp + os.Link publish-if-absent
// primitive used by boid's "load or generate and persist" secret files, so
// two daemon instances racing to boot against the same fresh data dir can
// never clobber each other's write (see docs/plans/volume-only-daemon.md
// §論点 d).
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
// This is publish-if-absent plus a safe read-back only; there is no repair
// path. An empty existing file, or any read-back error, is reported to the
// caller rather than acted on — every caller treats the files this
// publishes as volatile/regenerable, so failing and asking the operator to
// remove the stale artifact is an acceptable default.
//
// No crash-durability guarantee is made: Write+Close+Link can all report
// success while content still only lives in page cache, so a crash before
// writeback can still leave path short on the next boot.
func PublishIfAbsent(path string, perm os.FileMode, content []byte) ([]byte, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".atomicfile-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
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
		// A concurrent or earlier caller already published path first;
		// read back its content and return it as the winner.
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

// WriteAtomic atomically (over)writes content to path — write-temp +
// os.Rename, unconditional overwrite — for content that is always "the
// latest, correct content" rather than a value that must only ever be
// generated once (PublishIfAbsent's use case). Unlike a bare os.WriteFile,
// which truncates path in place, this guarantees any reader always sees
// either the complete old content or the complete new content, never a
// partial mix. path's parent directory must already exist.
func WriteAtomic(path string, perm os.FileMode, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".atomicfile-*.tmp")
	if err != nil {
		return fmt.Errorf("atomicfile: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomicfile: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomicfile: rename into place %s: %w", path, err)
	}
	return nil
}
