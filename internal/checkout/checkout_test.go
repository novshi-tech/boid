package checkout

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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

func TestCancelledCheckoutStopsWaitingForCacheLock(t *testing.T) {
	base, cache, root := upstream(t), t.TempDir(), t.TempDir()
	path := filepath.Join(cache, "host", "owner", "repo.git.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, "host/owner/repo", base, cache, root, &bytes.Buffer{}); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("checkout error=%v", err)
		}
	case <-time.After(time.Second):
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		<-done
		t.Fatal("cancelled checkout remains blocked by another job's cache lock")
	}
	if _, err := os.Stat(filepath.Join(root, "repo")); !os.IsNotExist(err) {
		t.Fatalf("cancelled destination remains: %v", err)
	}
}
