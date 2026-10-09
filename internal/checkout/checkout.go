// Package checkout creates independent worktrees using a shared git object cache.
package checkout

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ValidateRepo accepts a host/owner/repo identity safe for URLs and local paths.
func ValidateRepo(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 3 {
		return fmt.Errorf("repository must be host/owner/repo")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "-") {
			return fmt.Errorf("invalid repository component %q", part)
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
				return fmt.Errorf("invalid repository component %q", part)
			}
		}
	}
	return nil
}

// Run clones repo under root, retaining no dependency on the cache volume.
func Run(ctx context.Context, repo, base, cacheRoot, root string, warnings io.Writer) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("no primary repository: use boid checkout <name> for a registered repository")
	}
	if err := ValidateRepo(repo); err != nil {
		return "", err
	}
	if base == "" {
		return "", fmt.Errorf("BOID_GIT_BASE is required inside the sandbox")
	}
	dest := filepath.Join(root, filepath.Base(repo))
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", err
	}
	// Reserve the destination before touching the cache, so retries only remove our own files.
	if err := os.Mkdir(dest, 0755); err != nil {
		return "", fmt.Errorf("checkout destination %s must not exist: %w", dest, err)
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dest)
		}
	}()
	url := strings.TrimRight(base, "/") + "/" + repo + ".git"
	run := func(args ...string) error {
		c := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
		b, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git: %w: %s", err, strings.ReplaceAll(strings.TrimSpace(string(b)), base, "<git-base>"))
		}
		return nil
	}
	cache := ""
	if cacheRoot != "" {
		cache = filepath.Join(cacheRoot, filepath.FromSlash(repo)+".git")
		release, err := lockCache(ctx, cache)
		if err != nil {
			fmt.Fprintf(warnings, "warning: git cache unavailable: %v\n", err)
			cache = ""
		} else {
			defer release()
			if _, err := os.Stat(cache); os.IsNotExist(err) {
				err = run("-c", "url."+strings.TrimRight(base, "/")+"/.insteadOf=boid-checkout://", "clone", "--bare", "--", "boid-checkout://"+repo+".git", cache)
				if err != nil {
					fmt.Fprintf(warnings, "warning: cache clone failed: %v\n", err)
				}
			} else {
				if err := fetchCache(ctx, cache, root, url, base, run); err != nil {
					fmt.Fprintf(warnings, "warning: cache fetch failed: %v\n", err)
				}
			}
		}
	} else {
		fmt.Fprintln(warnings, "warning: BOID_GIT_CACHE is empty; cloning without cache")
	}
	if cache != "" {
		if err := run("clone", "--reference", cache, "--dissociate", "--", url, dest); err == nil {
			success = true
			return dest, nil
		} else {
			fmt.Fprintf(warnings, "warning: reference clone failed; retrying without cache: %v\n", err)
		}
		if err := os.RemoveAll(dest); err != nil {
			return "", err
		}
		if err := os.Mkdir(dest, 0755); err != nil {
			return "", err
		}
	}
	if err := run("clone", "--", url, dest); err != nil {
		return "", err
	}
	success = true
	return dest, nil
}

// fetchCache reads only cached objects, never the shared repository's config or
// hooks. A readonly job can modify that config (including URL rewrites, credential
// helpers and includes), so command-line hook overrides alone are insufficient.
// Refs are private: the shared repository is only an object cache for --reference.
func fetchCache(ctx context.Context, cache, root, url, base string, run func(...string) error) error {
	control, err := os.MkdirTemp(root, ".boid-cache-fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(control)
	if err := run("init", "--bare", "--template=", control); err != nil {
		return err
	}
	objects, err := filepath.Abs(filepath.Join(cache, "objects"))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath="+os.DevNull,
		"-c", "gc.auto=0", "-c", "maintenance.auto=false", "--git-dir="+control,
		"fetch", "--no-write-fetch-head", url, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	cmd.Env = append(os.Environ(), "GIT_OBJECT_DIRECTORY="+objects)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git cache fetch: %w: %s", err, strings.ReplaceAll(strings.TrimSpace(string(output)), base, "<git-base>"))
	}
	return nil
}
