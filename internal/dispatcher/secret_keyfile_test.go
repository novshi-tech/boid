package dispatcher_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
)

// TestLoadOrCreateKey_GeneratesAndPersists pins the on-first-boot
// generate path and its permissions/size.
func TestLoadOrCreateKey_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")

	key, err := dispatcher.LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key len = %d, want 32", len(key))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
}

// TestLoadOrCreateKey_ReusesExisting pins "既存を再利用": a second call
// against the same path must load the identical key, not regenerate.
func TestLoadOrCreateKey_ReusesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")

	first, err := dispatcher.LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("first LoadOrCreateKey: %v", err)
	}
	second, err := dispatcher.LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("second LoadOrCreateKey: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second call key differs from first (regenerated instead of reused)")
	}
}

// TestLoadOrCreateKey_CreatesMissingDir pins that the parent directory is
// created if needed, matching internal/install.LoadOrCreate's
// MkdirAll-then-generate shape.
func TestLoadOrCreateKey_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "boid")
	path := filepath.Join(dir, "secret.key")

	key, err := dispatcher.LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key len = %d, want 32", len(key))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected key file to exist under the created dir: %v", err)
	}
}

// TestLoadOrCreateKey_RejectsWrongLength pins the existing format-guard
// (unchanged by this PR — "Do NOT change the on-disk file format").
func TestLoadOrCreateKey_RejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(path, []byte("too-short"), 0o600); err != nil {
		t.Fatalf("seed short key file: %v", err)
	}

	if _, err := dispatcher.LoadOrCreateKey(path); err == nil {
		t.Fatal("LoadOrCreateKey succeeded against a non-32-byte file; want an error")
	}
}

// TestLoadOrCreateKey_ConcurrentStartup_SameKey pins the same hazard class
// internal/install.LoadOrCreate's own TestLoadOrCreate_ConcurrentStartup_SameID
// pins (Major 7, PR6 codex review) and internal/atomicfile.PublishIfAbsent
// now closes for this file too (docs/plans/volume-only-daemon.md §論点 d):
// two daemon instances racing to boot against the same fresh, empty named
// volume must agree on exactly one key.
func TestLoadOrCreateKey_ConcurrentStartup_SameKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	const n = 20

	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			keys[i], errs[i] = dispatcher.LoadOrCreateKey(path)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: LoadOrCreateKey: %v", i, err)
		}
	}
	want := keys[0]
	if len(want) != 32 {
		t.Fatalf("goroutine 0: key len = %d, want 32", len(want))
	}
	for i, key := range keys {
		if !bytes.Equal(key, want) {
			t.Errorf("goroutine %d: key = %x, want %x (every concurrent caller must agree on one key)", i, key, want)
		}
	}
}
