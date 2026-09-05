package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/atomicfile"
)

// LoadOrCreateKey loads the master key from the given path, or atomically
// generates and persists a fresh one if it doesn't exist yet (see
// docs/plans/volume-only-daemon.md §論点 d for the concurrent-boot rationale).
func LoadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		// Reject a pre-seeded key file with broader-than-0600 permissions —
		// reusing one would silently expose signing/encryption material.
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
