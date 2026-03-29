package secret

import (
	"fmt"
	"os"
	"path/filepath"
)

// LoadOrCreateKey loads the master key from the given path,
// or creates a new one if it doesn't exist.
func LoadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != 32 {
			return nil, fmt.Errorf("invalid key file: expected 32 bytes, got %d", len(data))
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key: %w", err)
	}

	// Create new key
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	key := GenerateKey()
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	return key, nil
}
