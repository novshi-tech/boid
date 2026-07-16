package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ErrDefaultHarnessNotSet is returned by DefaultHarness when no default
// harness has been configured via env var or config file.
var ErrDefaultHarnessNotSet = errors.New("default harness not set")

// EnvDefaultHarness is the environment variable that overrides the
// configured default harness.
const EnvDefaultHarness = "BOID_DEFAULT_HARNESS"

// validHarnessName checks the cosmetic shape of a harness identifier.
// It does not enforce a hard-coded enum so new harnesses can be added without
// touching this package; installed-binary checks live elsewhere.
var validHarnessName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// ValidateHarnessName returns an error if s is not a syntactically valid
// harness identifier. It does NOT verify the binary is installed.
func ValidateHarnessName(s string) error {
	if s == "" {
		return fmt.Errorf("harness name must not be empty")
	}
	if !validHarnessName.MatchString(s) {
		return fmt.Errorf("invalid harness name %q: must match [a-zA-Z][a-zA-Z0-9_-]*", s)
	}
	return nil
}

// DefaultHarness resolves the default harness identifier using:
//
//  1. env var BOID_DEFAULT_HARNESS, if non-empty
//  2. config file (~/.config/boid/config.yaml) default_harness key
//
// It returns ErrDefaultHarnessNotSet when neither source supplies a value, so
// callers can branch on errors.Is(err, ErrDefaultHarnessNotSet) and prompt
// the user.
func DefaultHarness() (string, error) {
	if v := os.Getenv(EnvDefaultHarness); v != "" {
		if err := ValidateHarnessName(v); err != nil {
			return "", fmt.Errorf("%s: %w", EnvDefaultHarness, err)
		}
		return v, nil
	}
	cfg, err := Load()
	if err != nil {
		return "", err
	}
	if cfg.DefaultHarness == "" {
		return "", ErrDefaultHarnessNotSet
	}
	if err := ValidateHarnessName(cfg.DefaultHarness); err != nil {
		return "", fmt.Errorf("default_harness in config: %w", err)
	}
	return cfg.DefaultHarness, nil
}

// SetDefaultHarness persists harness as the default_harness in the user's
// config file (~/.config/boid/config.yaml). Existing keys are preserved by
// reading the raw YAML, mutating the default_harness key, and re-marshalling.
// The write is atomic: a sibling temp file is written and renamed into place.
func SetDefaultHarness(harness string) error {
	if err := ValidateHarnessName(harness); err != nil {
		return err
	}
	path, err := defaultConfigPath()
	if err != nil {
		return err
	}
	return setDefaultHarnessAt(path, harness)
}

// defaultConfigPath returns the user-config path that Load uses.
func defaultConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(configDir, "boid", "config.yaml"), nil
}

// setDefaultHarnessAt is the testable form: the config path is a parameter.
func setDefaultHarnessAt(path, harness string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	raw := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) > 0 {
			if err := yaml.Unmarshal(data, &raw); err != nil {
				return fmt.Errorf("parse existing %s: %w", path, err)
			}
			if raw == nil {
				raw = map[string]any{}
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// fresh file
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}

	raw["default_harness"] = harness

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return WriteFileAtomic(path, out, 0o600)
}

// WriteFileAtomic writes data to path via a sibling temp file + rename, so
// concurrent readers (or a crash mid-write) never observe a partially
// written file. The temp file is created in the same directory as path (so
// the final os.Rename is same-filesystem and atomic), synced to disk before
// close, and removed on any error path before return — an existing file at
// path is left untouched unless the write fully succeeds.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config.yaml.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
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
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	cleanup = false
	return nil
}
