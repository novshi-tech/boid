//go:build linux

package checkout

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
