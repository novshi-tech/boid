package orchestrator

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkspaceStore provides persistence for WorkspaceMeta values.
// Each workspace is stored as a YAML file at <dir>/<slug>.yaml.
type WorkspaceStore struct {
	dir string
}

// NewWorkspaceStore returns a WorkspaceStore backed by dir.
// If dir is empty, DefaultWorkspaceDir is called to determine the directory.
// The directory is not created eagerly; it is created on the first Save call.
func NewWorkspaceStore(dir string) *WorkspaceStore {
	if dir == "" {
		d, err := DefaultWorkspaceDir()
		if err != nil {
			// Fall back to an empty string so callers fail clearly on use.
			d = ""
		}
		dir = d
	}
	return &WorkspaceStore{dir: dir}
}

// DefaultWorkspaceDir returns the default directory for workspace YAML files:
// $XDG_CONFIG_HOME/boid/workspaces, or ~/.config/boid/workspaces when
// XDG_CONFIG_HOME is unset (matching the behaviour of os.UserConfigDir on Linux).
func DefaultWorkspaceDir() (string, error) {
	// os.UserConfigDir already follows XDG_CONFIG_HOME on Linux and falls
	// back to $HOME/.config when the variable is unset.
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("workspace store: could not determine user config directory: %w", err)
	}
	return filepath.Join(configDir, "boid", "workspaces"), nil
}

// Load reads and parses the WorkspaceMeta for the given slug.
// Returns an error wrapping os.ErrNotExist when the file does not exist.
func (s *WorkspaceStore) Load(slug string) (*WorkspaceMeta, error) {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, slug+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
		}
		return nil, fmt.Errorf("workspace %q: read: %w", slug, err)
	}
	var meta WorkspaceMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("workspace %q: parse: %w", slug, err)
	}
	return &meta, nil
}

// Save atomically writes meta as YAML to <dir>/<slug>.yaml.
// The directory is created with mode 0755 if it does not exist.
// The write is atomic: the data is first written to a temporary file in the
// same directory and then renamed into place.
func (s *WorkspaceStore) Save(slug string, meta *WorkspaceMeta) error {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("workspace store: mkdir %q: %w", s.dir, err)
	}
	data, err := yaml.Marshal(meta)
	if err != nil {
		return fmt.Errorf("workspace %q: marshal: %w", slug, err)
	}

	// Write to a temporary file in the same directory, then rename atomically.
	tmpName := filepath.Join(s.dir, fmt.Sprintf("%s.yaml.tmp.%08x", slug, rand.Uint32()))
	if err := os.WriteFile(tmpName, data, 0o644); err != nil {
		return fmt.Errorf("workspace %q: write tmp: %w", slug, err)
	}
	dest := filepath.Join(s.dir, slug+".yaml")
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup
		return fmt.Errorf("workspace %q: rename: %w", slug, err)
	}
	return nil
}

// Remove deletes the YAML file for the given slug.
// Returns an error wrapping os.ErrNotExist when the file does not exist.
// It does not check whether any project references this workspace.
func (s *WorkspaceStore) Remove(slug string) error {
	if err := ValidWorkspaceSlug(slug); err != nil {
		return err
	}
	path := filepath.Join(s.dir, slug+".yaml")
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
		}
		return fmt.Errorf("workspace %q: remove: %w", slug, err)
	}
	return nil
}

// List returns the slug of every workspace stored in the directory, sorted
// alphabetically. If the directory does not exist, an empty slice and nil
// error are returned (degraded window).
func (s *WorkspaceStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("workspace store: list %q: %w", s.dir, err)
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		slug := strings.TrimSuffix(name, ".yaml")
		// Skip files whose base name is not a valid slug (e.g. tmp files).
		if ValidWorkspaceSlug(slug) == nil {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}
