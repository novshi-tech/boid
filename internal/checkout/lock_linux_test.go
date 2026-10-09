//go:build linux

package checkout

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

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

func TestUnsafeCacheRefsFallBackWithoutBlocking(t *testing.T) {
	for _, attack := range []string{"packed FIFO", "loose FIFO", "packed symlink", "loose symlink", "packed oversized", "loose oversized"} {
		t.Run(attack, func(t *testing.T) {
			base, cache := upstream(t), t.TempDir()
			if _, err := Run(context.Background(), "host/owner/repo", base, cache, t.TempDir(), &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			bare := filepath.Join(cache, "host/owner/repo.git")
			path := filepath.Join(bare, "packed-refs")
			if strings.HasPrefix(attack, "loose") {
				path = filepath.Join(bare, "refs/heads/main")
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			switch {
			case strings.HasSuffix(attack, "FIFO"):
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(attack, "symlink"):
				if err := os.Symlink("/dev/zero", path); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(attack, "oversized"):
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(maxCacheRefBytes + 1); err != nil {
					t.Fatal(err)
				}
				file.Close()
			}
			// Repeat against the same poisoned cache to verify the flock is released.
			for i := 0; i < 2; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				done := make(chan struct{})
				var out string
				var err error
				var warnings bytes.Buffer
				root := t.TempDir()
				go func() {
					out, err = Run(ctx, "host/owner/repo", base, cache, root, &warnings)
					close(done)
				}()
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("checkout blocked by unsafe cache ref")
				}
				if err != nil {
					t.Fatal(err)
				}
				verify(t, out)
				if !strings.Contains(warnings.String(), "invalid cache refs") || strings.Contains(warnings.String(), "reference clone") {
					t.Fatalf("expected cache rejection before reference clone: %s", &warnings)
				}
			}
		})
	}
}

func TestOpenCacheRefFileDoesNotBlockOnReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ref")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// Exercise the open used after Lstat directly, simulating a replaced file.
	done := make(chan error, 1)
	go func() {
		file, err := openCacheRefFile(path)
		if err == nil {
			file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("opening a replacement FIFO blocked")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", path); err != nil {
		t.Fatal(err)
	}
	if file, err := openCacheRefFile(path); err == nil {
		file.Close()
		t.Fatal("opening a replacement symlink succeeded")
	}
}
