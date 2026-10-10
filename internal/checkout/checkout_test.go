package checkout

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func git(t *testing.T, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
func upstream(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	git(t, "init", "-b", "main", src)
	if err := os.WriteFile(filepath.Join(src, "content"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", src, "add", ".")
	git(t, "-C", src, "commit", "-m", "initial")
	bare := filepath.Join(root, "host", "owner", "repo.git")
	git(t, "clone", "--bare", src, bare)
	return "file://" + root
}
func verify(t *testing.T, path string) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(path, "content")); err != nil || string(b) != "hello" {
		t.Fatalf("content: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(path, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("alternates remains: %v", err)
	}
	if got := git(t, "-C", path, "branch", "--show-current"); got != "main" {
		t.Fatalf("branch = %s", got)
	}
}
func TestConcurrentCheckoutDissociates(t *testing.T) {
	base, cache := upstream(t), t.TempDir()
	roots := []string{t.TempDir(), t.TempDir()}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	paths := make([]string, 2)
	warnings := make([]bytes.Buffer, 2)
	for i := range roots {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			paths[i], errs[i] = Run(context.Background(), "host/owner/repo", base, cache, roots[i], &warnings[i])
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range roots {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if warnings[i].Len() != 0 {
			t.Fatalf("concurrent cache warning: %s", &warnings[i])
		}
		verify(t, paths[i])
	}
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		git(t, "-C", path, "fsck", "--full")
	}
}
func TestCorruptCacheFallsBack(t *testing.T) {
	base, cache, root := upstream(t), t.TempDir(), t.TempDir()
	path := filepath.Join(cache, "host", "owner", "repo.git")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "garbage"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	out, err := Run(context.Background(), "host/owner/repo", base, cache, root, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, out)
	if !strings.Contains(warnings.String(), "reference") {
		t.Fatalf("no fallback warning: %s", warnings.String())
	}
}
func TestCheckoutPreservesDestination(t *testing.T) {
	base, cache, root := upstream(t), t.TempDir(), t.TempDir()
	path := filepath.Join(root, "repo")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), "host/owner/repo", base, cache, root, &bytes.Buffer{}); err == nil {
		t.Fatal("existing destination accepted")
	}
	if b, err := os.ReadFile(filepath.Join(path, "keep")); err != nil || string(b) != "keep" {
		t.Fatal("destination modified")
	}
}
func TestCheckoutMissingCacheStillClones(t *testing.T) {
	var warnings bytes.Buffer
	out, err := Run(context.Background(), "host/owner/repo", upstream(t), "", t.TempDir(), &warnings)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, out)
}
func TestRepoValidation(t *testing.T) {
	for _, repo := range []string{"", "host/../repo", "host/owner/..", "host/owner/repo/extra", "host/owner/-repo", "host/owner/r%2f", "/owner/repo"} {
		if _, err := Run(context.Background(), repo, "file:///missing", "", t.TempDir(), &bytes.Buffer{}); err == nil {
			t.Errorf("accepted %q", repo)
		}
	}
}

func TestCacheDoesNotPersistJobToken(t *testing.T) {
	base, cache := upstream(t), t.TempDir()
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), "host/owner/repo", base, cache, t.TempDir(), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"config", "FETCH_HEAD"} {
		b, err := os.ReadFile(filepath.Join(cache, "host", "owner", "repo.git", file))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(base)) {
			t.Fatalf("job URL persisted in cache %s: %s", file, b)
		}
	}
}

func TestCacheCannotExecuteJobCode(t *testing.T) {
	for _, attack := range []string{"hooks", "hooksPath", "urlRewrite", "fsmonitor"} {
		t.Run(attack, func(t *testing.T) {
			base, cache := upstream(t), t.TempDir()
			if _, err := Run(context.Background(), "host/owner/repo", base, cache, t.TempDir(), &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			bare := filepath.Join(cache, "host", "owner", "repo.git")
			marker := filepath.Join(t.TempDir(), "executed")
			script := filepath.Join(t.TempDir(), "attack")
			if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			switch attack {
			case "hooks", "hooksPath":
				hooks := filepath.Join(bare, "hooks")
				if attack == "hooksPath" {
					hooks = t.TempDir()
					git(t, "-C", bare, "config", "core.hooksPath", hooks)
				}
				for _, hook := range []string{"reference-transaction", "post-fetch", "pre-auto-gc"} {
					if err := os.WriteFile(filepath.Join(hooks, hook), []byte("#!/bin/sh\n'"+script+"'\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "urlRewrite":
				git(t, "-C", bare, "config", "protocol.ext.allow", "always")
				git(t, "-C", bare, "config", "url.ext::"+script+".insteadOf", base)
			case "fsmonitor":
				git(t, "-C", bare, "config", "core.fsmonitor", script)
			}
			// Make the fetch update refs and acquire new objects, so this also
			// exercises reference-transaction hooks rather than a no-op fetch.
			src := strings.TrimPrefix(base, "file://") + "/src"
			git(t, "-C", src, "commit", "--allow-empty", "-m", "next")
			git(t, "-C", src, "push", strings.TrimPrefix(base, "file://")+"/host/owner/repo.git", "main")
			var warnings bytes.Buffer
			out, err := Run(context.Background(), "host/owner/repo", base, cache, t.TempDir(), &warnings)
			if err != nil {
				t.Fatal(err)
			}
			if warnings.Len() != 0 {
				t.Fatalf("cache refresh failed: %s", &warnings)
			}
			verify(t, out)
			if got, want := git(t, "-C", out, "rev-parse", "HEAD"), git(t, "-C", src, "rev-parse", "HEAD"); got != want {
				t.Fatalf("checkout = %s, want %s", got, want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("cache executed code: %v", err)
			}
		})
	}
}

func TestCacheCloneDisablesTemplateHooks(t *testing.T) {
	base := upstream(t)
	template := t.TempDir()
	if err := os.Mkdir(filepath.Join(template, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	if err := os.WriteFile(filepath.Join(template, "hooks", "reference-transaction"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_TEMPLATE_DIR", template)
	var warnings bytes.Buffer
	out, err := Run(context.Background(), "host/owner/repo", base, t.TempDir(), t.TempDir(), &warnings)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, out)
	if warnings.Len() != 0 {
		t.Fatalf("cache clone failed: %s", &warnings)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("template hook executed: %v", err)
	}
}

func TestCacheRefreshNegotiatesAndTracksRefs(t *testing.T) {
	base, cacheRoot := upstream(t), t.TempDir()
	up := strings.TrimPrefix(base, "file://") + "/host/owner/repo.git"
	src := strings.TrimPrefix(base, "file://") + "/src"
	data := make([]byte, 256*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "large"), data, 0600); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", src, "add", ".")
	git(t, "-C", src, "commit", "-m", "large payload")
	git(t, "-C", src, "push", up, "main")
	transfer := filepath.Join(t.TempDir(), "pack")
	hook := filepath.Join(t.TempDir(), "pack-hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n\"$@\" | tee '"+transfer+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config")
	git(t, "config", "--file", config, "uploadpack.packObjectsHook", hook)
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	checkout := func() {
		t.Helper()
		var warnings bytes.Buffer
		if _, err := Run(context.Background(), "host/owner/repo", base, cacheRoot, t.TempDir(), &warnings); err != nil {
			t.Fatal(err)
		}
		if warnings.Len() != 0 {
			t.Fatalf("warnings: %s", &warnings)
		}
	}
	checkout()
	initial, err := os.Stat(transfer)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(cacheRoot, "host/owner/repo.git")
	packStats := func() (int, int64) {
		t.Helper()
		files, err := filepath.Glob(filepath.Join(cache, "objects/pack/*.pack"))
		if err != nil {
			t.Fatal(err)
		}
		var size int64
		for _, file := range files {
			st, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			size += st.Size()
		}
		return len(files), size
	}
	count, size := packStats()
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(transfer, nil, 0600); err != nil {
			t.Fatal(err)
		}
		checkout()
		if n, s := packStats(); n != count || s != size {
			t.Fatalf("unchanged refresh grew packs: %d/%d -> %d/%d", count, size, n, s)
		}
		st, err := os.Stat(transfer)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() != 0 {
			t.Fatalf("unchanged refresh transferred %d bytes", st.Size())
		}
	}
	git(t, "-C", src, "commit", "--allow-empty", "-m", "next")
	git(t, "-C", src, "tag", "next")
	git(t, "-C", src, "push", up, "main", "refs/tags/next")
	checkout()
	if got, want := git(t, "-C", cache, "rev-parse", "main"), git(t, "-C", src, "rev-parse", "HEAD"); got != want {
		t.Fatalf("cache main = %s, want %s", got, want)
	}
	if got, want := git(t, "-C", cache, "rev-parse", "next"), git(t, "-C", src, "rev-parse", "next"); got != want {
		t.Fatalf("cache tag = %s, want %s", got, want)
	}
	incremental, err := os.Stat(transfer)
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Size() == 0 || incremental.Size()*10 >= initial.Size() {
		t.Fatalf("transfer initial=%d incremental=%d", initial.Size(), incremental.Size())
	}
	t.Logf("pack transfer: initial=%d bytes, incremental=%d bytes", initial.Size(), incremental.Size())
	count, size = packStats()
	checkout()
	if n, s := packStats(); n != count || s != size {
		t.Fatalf("updated refresh grew packs: %d/%d -> %d/%d", count, size, n, s)
	}
}

func TestCacheRefsRejectUntrustedInput(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, line := range []string{
		hash + " refs/heads/main\nupdate refs/heads/injected " + hash,
		"not-a-hash refs/heads/main",
		hash + " refs/heads/../../config",
		hash + " refs/heads/main.lock",
		hash + " refs/heads/-bad\toption no-deref",
	} {
		t.Run(line, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(line), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCacheRefs(dir); err == nil {
				t.Fatal("accepted unsafe cache refs")
			}
		})
	}
	dir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "refs")); err != nil {
		t.Fatal(err)
	}
	if _, err := readCacheRefs(dir); err == nil {
		t.Fatal("accepted symlinked cache refs")
	}
}

func TestCacheLooseRefsOverridePackedRefs(t *testing.T) {
	dir := t.TempDir()
	old, current := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte("# pack-refs with: peeled\n"+old+" refs/heads/main\n"+old+" refs/tags/release\n^"+old+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs/heads"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "refs/heads/main"), []byte(current+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	refs, err := readCacheRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs["refs/heads/main"] != current || refs["refs/tags/release"] != old {
		t.Fatalf("refs = %v", refs)
	}
}

func TestRepoPortValidation(t *testing.T) {
	for _, repo := range []string{"host:443/owner/repo", "host.docker.internal:42369/owner/repo"} {
		if err := ValidateRepo(repo); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
	}
	for _, repo := range []string{"host:/owner/repo", "host:0/owner/repo", "host:65536/owner/repo", "host:+443/owner/repo", "host:443:80/owner/repo", "host:443/owner:80/repo", "host:443/owner/repo:80"} {
		if err := ValidateRepo(repo); err == nil {
			t.Errorf("accepted %q", repo)
		}
	}
}
