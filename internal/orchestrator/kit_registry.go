package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
)

// KitRegistry manages installed kits under a base directory.
// Each kit is a directory ~/.local/share/boid/kits/<name>/ containing kit.yaml.
type KitRegistry struct {
	BaseDir string // e.g. ~/.local/share/boid/kits
}

// NewRegistry creates a new kit registry with the given base directory.
func NewRegistry(baseDir string) *KitRegistry {
	return &KitRegistry{BaseDir: baseDir}
}

// Resolve returns the absolute filesystem path for a kit directory.
// The name must be a valid kit name (see ValidKitName).
func (r *KitRegistry) Resolve(name string) (string, error) {
	if err := ValidKitName(name); err != nil {
		return "", fmt.Errorf("kit %q: %w", name, err)
	}
	dir := filepath.Join(r.BaseDir, name)
	yamlPath := filepath.Join(dir, "kit.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		return "", fmt.Errorf("kit %q: kit.yaml not found at %s", name, dir)
	}
	return dir, nil
}

// IsInstalled returns true if the kit directory exists under BaseDir.
func (r *KitRegistry) IsInstalled(name string) bool {
	dest := filepath.Join(r.BaseDir, name)
	_, err := os.Stat(dest)
	return err == nil
}

// List returns all installed kit names.
// It finds directories that contain kit.yaml directly under BaseDir.
// If BaseDir does not exist, an empty slice and nil error are returned.
func (r *KitRegistry) List() ([]string, error) {
	entries, err := os.ReadDir(r.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		yamlPath := filepath.Join(r.BaseDir, e.Name(), "kit.yaml")
		if _, err := os.Stat(yamlPath); err == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Remove deletes an installed kit directory.
func (r *KitRegistry) Remove(name string) error {
	dest := filepath.Join(r.BaseDir, name)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return fmt.Errorf("kit %q not installed", name)
	}
	return os.RemoveAll(dest)
}
