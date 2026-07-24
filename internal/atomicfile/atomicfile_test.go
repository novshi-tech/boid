package atomicfile_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/atomicfile"
)

// TestPublishIfAbsent_CreatesFileWithContentAndMode pins the basic
// happy-path contract: content lands on disk verbatim, at the requested
// permission bits.
func TestPublishIfAbsent_CreatesFileWithContentAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	got, err := atomicfile.PublishIfAbsent(path, 0o600, []byte("hello"))
	if err != nil {
		t.Fatalf("PublishIfAbsent: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("returned content = %q, want %q", got, "hello")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("file content = %q, want %q", data, "hello")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
}

// TestPublishIfAbsent_NoTempLeftover pins that the temp file used to
// stage the write is always cleaned up — both on the success path (where
// os.Link leaves a second, now-redundant name behind) and generally.
func TestPublishIfAbsent_NoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	if _, err := atomicfile.PublishIfAbsent(path, 0o600, []byte("x")); err != nil {
		t.Fatalf("PublishIfAbsent: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want exactly 1 (no leftover temp file): %v", len(entries), entries)
	}
}

// TestPublishIfAbsent_SecondCallDoesNotOverwrite pins the "load or
// create" contract this primitive exists for: a second PublishIfAbsent
// call against a path that already has content must not replace it —
// every caller returns the FIRST publish's content instead of silently
// clobbering it with a fresh, different generated value.
func TestPublishIfAbsent_SecondCallDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	first, err := atomicfile.PublishIfAbsent(path, 0o600, []byte("first"))
	if err != nil {
		t.Fatalf("first PublishIfAbsent: %v", err)
	}

	second, err := atomicfile.PublishIfAbsent(path, 0o600, []byte("second"))
	if err != nil {
		t.Fatalf("second PublishIfAbsent: %v", err)
	}
	if !bytes.Equal(second, first) {
		t.Errorf("second call content = %q, want the already-published %q (must not overwrite)", second, first)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first" {
		t.Errorf("on-disk content = %q, want %q (first publish wins)", data, "first")
	}
}

// TestPublishIfAbsent_RepairsStaleEmptyFile pins the crash-recovery edge
// case install_id.go's own readValidInstallID/rename-repair logic
// documents: a path that exists but holds no legitimate content (e.g.
// truncated by a crash mid-write under some OTHER writer that predates
// this atomic protocol) has no live concurrent claim on it, so it is
// safe to replace outright via os.Rename rather than treated as an
// already-published winner.
func TestPublishIfAbsent_RepairsStaleEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}

	got, err := atomicfile.PublishIfAbsent(path, 0o600, []byte("repaired"))
	if err != nil {
		t.Fatalf("PublishIfAbsent: %v", err)
	}
	if string(got) != "repaired" {
		t.Errorf("content = %q, want %q", got, "repaired")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "repaired" {
		t.Errorf("on-disk content = %q, want %q", data, "repaired")
	}
}

// TestPublishIfAbsent_ConcurrentRace_OneWinnerNoPartialRead pins the
// actual hazard class this primitive exists to close (mirrors
// internal/install.LoadOrCreate's own TestLoadOrCreate_ConcurrentStartup_SameID,
// Major 7 PR6 codex review): N goroutines racing PublishIfAbsent against
// the same fresh path — on a fresh, empty named volume at daemon boot,
// exactly this scenario — must never error (which would happen if a
// goroutine observed a half-written file) and must all agree on exactly
// one winning content.
func TestPublishIfAbsent_ConcurrentRace_OneWinnerNoPartialRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	const n = 20

	results := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			content := []byte(fmt.Sprintf("content-%d", i))
			results[i], errs[i] = atomicfile.PublishIfAbsent(path, 0o600, content)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: PublishIfAbsent: %v", i, err)
		}
	}
	want := results[0]
	for i, got := range results {
		if !bytes.Equal(got, want) {
			t.Errorf("goroutine %d result = %q, want %q (every concurrent caller must agree on one winner)", i, got, want)
		}
	}
}
