package mixin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Registry manages installed mixin repositories under a base directory.
type Registry struct {
	BaseDir string // e.g. ~/.local/share/boid/mixins
}

// NewRegistry creates a new Registry with the given base directory.
func NewRegistry(baseDir string) *Registry {
	return &Registry{BaseDir: baseDir}
}

// Resolve returns the absolute filesystem path for a mixin reference.
// A ref like "github.com/user/repo/go" is split into the repo path
// (first 3 segments) and the mixin subpath (remainder).
func (r *Registry) Resolve(ref string) (string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) < 4 {
		return "", fmt.Errorf("mixin ref %q: need at least host/owner/repo/mixin", ref)
	}

	dir := filepath.Join(r.BaseDir, ref)
	yamlPath := filepath.Join(dir, "mixin.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		return "", fmt.Errorf("mixin %q: mixin.yaml not found at %s", ref, dir)
	}
	return dir, nil
}

// Install clones a mixin repository from its conventional URL.
// The repoRef should be like "github.com/user/repo".
func (r *Registry) Install(repoRef string) error {
	url := "https://" + repoRef + ".git"
	return r.InstallFromURL(repoRef, url)
}

// InstallFromURL clones a git repo from the given URL into BaseDir/repoRef.
func (r *Registry) InstallFromURL(repoRef, url string) error {
	dest := filepath.Join(r.BaseDir, repoRef)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("mixin repo %q already installed at %s", repoRef, dest)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	cmd := exec.Command("git", "clone", url, dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s: %s", url, out)
	}
	return nil
}

// List returns all installed mixin repository references.
// It finds directories containing .git under BaseDir.
func (r *Registry) List() ([]string, error) {
	var repos []string
	err := filepath.WalkDir(r.BaseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			rel, _ := filepath.Rel(r.BaseDir, filepath.Dir(path))
			repos = append(repos, rel)
			return filepath.SkipDir
		}
		return nil
	})
	return repos, err
}

// Remove deletes an installed mixin repository.
func (r *Registry) Remove(repoRef string) error {
	dest := filepath.Join(r.BaseDir, repoRef)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return fmt.Errorf("mixin repo %q not installed", repoRef)
	}
	return os.RemoveAll(dest)
}

// Update runs git pull in an installed mixin repository.
func (r *Registry) Update(repoRef string) error {
	dest := filepath.Join(r.BaseDir, repoRef)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return fmt.Errorf("mixin repo %q not installed", repoRef)
	}

	cmd := exec.Command("git", "-C", dest, "pull")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git pull in %s: %s", dest, out)
	}
	return nil
}
