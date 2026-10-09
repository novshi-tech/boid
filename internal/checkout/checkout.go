// Package checkout creates independent worktrees using a shared git object cache.
package checkout

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
		c := gitCommand(ctx, args...)
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

// fetchCache refreshes shared objects and refs through a private repository.
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
	refs, err := readCacheRefs(cache)
	if err != nil {
		return err
	}
	var updates strings.Builder
	for name, hash := range refs {
		fmt.Fprintf(&updates, "update %s %s\n", name, hash)
	}
	cmd := gitCommand(ctx, "--git-dir="+control, "update-ref", "--stdin")
	cmd.Env = append(os.Environ(), "GIT_OBJECT_DIRECTORY="+objects)
	cmd.Stdin = strings.NewReader(updates.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("seed cache refs: %w: %s", err, output)
	}
	cmd = gitCommand(ctx, "--git-dir="+control,
		"fetch", "--no-write-fetch-head", url, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	cmd.Env = append(os.Environ(), "GIT_OBJECT_DIRECTORY="+objects)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git cache fetch: %w: %s", err, strings.ReplaceAll(strings.TrimSpace(string(output)), base, "<git-base>"))
	}
	refs, err = readCacheRefs(control)
	if err != nil {
		return err
	}
	return writeCacheRefs(cache, refs)
}

// gitCommand applies the same execution overrides to every checkout command.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
}

// validCacheRef accepts direct SHA-1 branch and tag refs suitable for update-ref.
func validCacheRef(name, hash string) bool {
	if len(hash) != 40 || hash == strings.Repeat("0", 40) {
		return false
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return false
	}
	if !strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/tags/") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.ContainsAny(name, " ~^:?*[\\") {
		return false
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// readCacheRefs reads validated packed and loose refs without loading Git config.
func readCacheRefs(dir string) (map[string]string, error) {
	refs := make(map[string]string)
	packed, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(packed)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validCacheRef(fields[1], fields[0]) {
			return nil, fmt.Errorf("invalid packed cache ref")
		}
		refs[fields[1]] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	err = filepath.WalkDir(filepath.Join(dir, "refs"), func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in cache refs")
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hash := strings.TrimSpace(string(data))
		if !validCacheRef(name, hash) {
			return fmt.Errorf("invalid loose cache ref")
		}
		refs[name] = hash
		return nil
	})
	return refs, err
}

// writeCacheRefs publishes fetched tips without executing Git in the shared cache.
func writeCacheRefs(cache string, refs map[string]string) error {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var packed strings.Builder
	for _, name := range names {
		fmt.Fprintf(&packed, "%s %s\n", refs[name], name)
	}
	file, err := os.CreateTemp(cache, ".boid-packed-refs-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	if _, err := file.WriteString(packed.String()); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(cache, "packed-refs")); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(cache, "refs")); err != nil {
		return err
	}
	return os.Mkdir(filepath.Join(cache, "refs"), 0755)
}
