// Package checkout creates independent worktrees using a shared git object cache.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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
		c := exec.CommandContext(ctx, "git", args...)
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
				if err := run("-C", cache, "fetch", "--prune", "--no-write-fetch-head", url, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
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

// lockCache uses a sibling lock file so the bare repository may be created or repaired.
func lockCache(ctx context.Context, cache string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(cache), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(cache+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
