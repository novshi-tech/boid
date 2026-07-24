package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/atomicfile"
)

// LoadOrCreateKey loads the master key from the given path, or atomically
// generates and persists a fresh one if it doesn't exist yet.
//
// Publish uses atomicfile.PublishIfAbsent (docs/plans/volume-only-daemon.md
// §論点 d — the same write-temp + os.Link protocol internal/install.
// LoadOrCreate pioneered, Major 7 PR6 codex review) instead of a plain
// os.WriteFile: two daemon instances racing to boot against the same
// fresh, empty data dir (a named volume with nothing in it yet — the
// scenario this file and web_secret both share as callers of this same
// function) must agree on exactly one key instead of each generating its
// own and clobbering the other's write, with a window where a reader
// could observe a half-written (or, worse, zero-byte and therefore
// format-invalid per the length check below) file in between.
func LoadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		// [Major 3, PR829 round 1 codex review]: a pre-seeded key file
		// (e.g. restored from an archive, or pre-seeded by a k8s
		// initContainer per docs/plans/volume-only-daemon.md §論点 d's own
		// "file が既にあれば読む" contract) is accepted without ever having
		// been published at 0600 by this function's own
		// atomicfile.PublishIfAbsent call below — reusing one with broader
		// permissions would silently expose signing/encryption material to
		// other users. Mirrors internal/mtls.LoadOrCreate's existing check
		// for ca.key.
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("stat key: %w", statErr)
		}
		if info.Mode().Perm()&0o177 != 0 {
			return nil, fmt.Errorf("dispatcher: key file %s has unsafe permissions %#o (must be 0600 — same as create-time)", path, info.Mode().Perm())
		}
		if len(data) != 32 {
			return nil, fmt.Errorf("invalid key file: expected 32 bytes, got %d", len(data))
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	key := GenerateKey()
	published, err := atomicfile.PublishIfAbsent(path, 0o600, key)
	if err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	if len(published) != 32 {
		return nil, fmt.Errorf("invalid key file: expected 32 bytes, got %d", len(published))
	}
	return published, nil
}
