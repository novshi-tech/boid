package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// KitRegistry manages installed kit repositories under a base directory.
type KitRegistry struct {
	BaseDir string // e.g. ~/.local/share/boid/kits
}

// NewRegistry creates a new kit registry with the given base directory.
func NewRegistry(baseDir string) *KitRegistry {
	return &KitRegistry{BaseDir: baseDir}
}

// Resolve returns the absolute filesystem path for a kit reference.
// A ref like "github.com/user/repo/go" is split into the repo path
// (first 3 segments) and the kit subpath (remainder).
func (r *KitRegistry) Resolve(ref string) (string, error) {
	if strings.HasPrefix(ref, "local/") {
		dir := filepath.Join(r.BaseDir, ref)
		yamlPath := filepath.Join(dir, "kit.yaml")
		if _, err := os.Stat(yamlPath); err != nil {
			return "", fmt.Errorf("kit %q: kit.yaml not found at %s", ref, dir)
		}
		return dir, nil
	}

	parts := strings.Split(ref, "/")
	if len(parts) < 4 {
		return "", fmt.Errorf("kit ref %q: need at least host/owner/repo/kit", ref)
	}

	dir := filepath.Join(r.BaseDir, ref)
	yamlPath := filepath.Join(dir, "kit.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		return "", fmt.Errorf("kit %q: kit.yaml not found at %s", ref, dir)
	}
	return dir, nil
}

// IsInstalled returns true if the repo directory already exists under BaseDir.
func (r *KitRegistry) IsInstalled(repoRef string) bool {
	dest := filepath.Join(r.BaseDir, repoRef)
	_, err := os.Stat(dest)
	return err == nil
}

// RepoRefsFromKitRefs extracts unique repo references (host/owner/repo)
// from a list of kit refs. Only remote refs with 4+ path segments are
// included. Local refs (< 4 segments) and "local/" prefix refs are skipped.
func RepoRefsFromKitRefs(kitRefs []string) []string {
	seen := make(map[string]struct{})
	var repos []string
	for _, ref := range kitRefs {
		if strings.HasPrefix(ref, "local/") {
			continue
		}
		parts := strings.Split(ref, "/")
		if len(parts) < 4 {
			continue
		}
		repo := strings.Join(parts[:3], "/")
		if _, ok := seen[repo]; ok {
			continue
		}
		seen[repo] = struct{}{}
		repos = append(repos, repo)
	}
	return repos
}

// RepoRefToCloneURL converts a repo reference to a git clone URL.
// When useSSH is true, it returns an SSH URL (git@host:owner/repo.git).
// Otherwise it returns an HTTPS URL (https://host/owner/repo.git).
func RepoRefToCloneURL(repoRef string, useSSH bool) string {
	if useSSH {
		host, path, _ := strings.Cut(repoRef, "/")
		return "git@" + host + ":" + path + ".git"
	}
	return "https://" + repoRef + ".git"
}

// Install clones a kit repository from its conventional URL.
// The repoRef should be like "github.com/user/repo".
// When useSSH is true, the SSH protocol (git@host:path.git) is used.
func (r *KitRegistry) Install(repoRef string, useSSH bool) error {
	url := RepoRefToCloneURL(repoRef, useSSH)
	return r.InstallFromURL(repoRef, url)
}

// InstallFromURL clones a git repo from the given URL into BaseDir/repoRef.
func (r *KitRegistry) InstallFromURL(repoRef, url string) error {
	dest := filepath.Join(r.BaseDir, repoRef)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("kit repo %q already installed at %s", repoRef, dest)
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

// List returns all installed kit repository references.
// It finds directories containing .git under BaseDir.
func (r *KitRegistry) List() ([]string, error) {
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

// Remove deletes an installed kit repository.
func (r *KitRegistry) Remove(repoRef string) error {
	dest := filepath.Join(r.BaseDir, repoRef)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return fmt.Errorf("kit repo %q not installed", repoRef)
	}
	return os.RemoveAll(dest)
}

// Update runs git pull in an installed kit repository.
func (r *KitRegistry) Update(repoRef string) error {
	dest := filepath.Join(r.BaseDir, repoRef)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return fmt.Errorf("kit repo %q not installed", repoRef)
	}

	cmd := exec.Command("git", "-C", dest, "pull")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git pull in %s: %s", dest, out)
	}
	return nil
}
