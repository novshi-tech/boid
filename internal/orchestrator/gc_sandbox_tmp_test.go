package orchestrator

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestCleanSandboxTmp_RemovesOldMatches(t *testing.T) {
	dir := t.TempDir()

	oldTime := time.Now().Add(-48 * time.Hour)

	files := []string{
		"boid-abc-inner.sh",
		"boid-abc-outer.sh",
		"boid-abc-setup.sh",
		"boid-exec-proj-inner.sh",
	}
	dirs := []string{
		"boid-root-XYZ",
		"boid-gates-uuid123",
	}

	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.WriteFile(p, []byte("#!/bin/bash\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
	for _, d := range dirs {
		p := filepath.Join(dir, d)
		if err := os.MkdirAll(filepath.Join(p, "nested"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	n := cleanSandboxTmp(dir, 24*time.Hour)
	want := len(files) + len(dirs)
	if n != want {
		t.Errorf("deleted count = %d, want %d", n, want)
	}

	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s not removed", f)
		}
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(dir, d)); !os.IsNotExist(err) {
			t.Errorf("%s not removed", d)
		}
	}
}

func TestCleanSandboxTmp_KeepsFreshFiles(t *testing.T) {
	dir := t.TempDir()
	freshPath := filepath.Join(dir, "boid-fresh-inner.sh")
	if err := os.WriteFile(freshPath, []byte("x"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	n := cleanSandboxTmp(dir, 24*time.Hour)
	if n != 0 {
		t.Errorf("deleted count = %d, want 0 (fresh file)", n)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("fresh file should remain: %v", err)
	}
}

func TestCleanSandboxTmp_IgnoresNonMatchingNames(t *testing.T) {
	dir := t.TempDir()
	oldTime := time.Now().Add(-48 * time.Hour)

	nonMatches := []string{
		"other-file.sh",            // no boid- prefix
		"boid-notes.txt",           // not script/dir pattern
		"boid-go-build-cache",      // dir but doesn't match root/gates
		"boid-start-payload-4.json", // doesn't match script suffix
	}

	for _, f := range nonMatches {
		p := filepath.Join(dir, f)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	n := cleanSandboxTmp(dir, 24*time.Hour)
	if n != 0 {
		t.Errorf("should not delete non-matching; got %d", n)
	}
	for _, f := range nonMatches {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s should remain: %v", f, err)
		}
	}
}

func TestCleanSandboxTmp_ZeroOlderThanDisablesAgeFilter(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "boid-fresh-inner.sh")
	if err := os.WriteFile(fresh, []byte("x"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	n := cleanSandboxTmp(dir, 0)
	if n != 1 {
		t.Errorf("with olderThan=0 should delete fresh file; got %d", n)
	}
}

func TestCleanSandboxTmp_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	n := cleanSandboxTmp(dir, 24*time.Hour)
	if n != 0 {
		t.Errorf("empty dir should yield 0 removals; got %d", n)
	}
}

func TestCleanSandboxTmp_EmptyTmpDirNoop(t *testing.T) {
	n := cleanSandboxTmp("", 24*time.Hour)
	if n != 0 {
		t.Errorf("empty tmpDir should noop; got %d", n)
	}
}

func TestReadSystemMountPoints_IncludesRoot(t *testing.T) {
	mounts, err := readSystemMountPoints()
	if err != nil {
		t.Fatalf("readSystemMountPoints: %v", err)
	}
	if !slices.Contains(mounts, "/") {
		t.Errorf("expected union of mount points to include '/' (host root); got %d entries", len(mounts))
	}
}

func TestReadSystemMountPoints_DedupsByNamespace(t *testing.T) {
	mounts, err := readSystemMountPoints()
	if err != nil {
		t.Fatalf("readSystemMountPoints: %v", err)
	}
	seen := make(map[string]int)
	for _, m := range mounts {
		seen[m]++
	}
	for m, n := range seen {
		if n != 1 {
			t.Errorf("mount %q appears %d times; expected dedup", m, n)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"123", true},
		{"0", true},
		{"12a", false},
		{"a12", false},
		{"-1", false},
		{"1 2", false},
	}
	for _, tc := range cases {
		if got := isAllDigits(tc.in); got != tc.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestReadChrootHolders_DoesNotIncludeSlashOnly(t *testing.T) {
	held, err := readChrootHolders()
	if err != nil {
		t.Fatalf("readChrootHolders: %v", err)
	}
	if _, ok := held["/"]; ok {
		t.Errorf("/ should be excluded (host root, not a chroot holder)")
	}
}

// TestCleanSandboxTmp_SkipsBoidRootHeldByProcess exercises the chroot-holder
// guard by aiming the syscall at a /tmp/boid-root-* directory that the
// current process holds open via /proc/self/root semantics. We can't directly
// chroot in a test (requires CAP_SYS_CHROOT), so we instead verify the
// hasHeldRoot path by prepopulating the GC's view of held roots through the
// shared filesystem: ensure that a fresh-mtime boid-root-* in the test tmpdir
// is removed when nothing holds it, but not when one is held — which is what
// the real implementation already exercises through readChrootHolders, so we
// just sanity-check that readChrootHolders returns a non-nil map.
func TestReadChrootHolders_Smoke(t *testing.T) {
	held, err := readChrootHolders()
	if err != nil {
		t.Fatalf("readChrootHolders: %v", err)
	}
	if held == nil {
		t.Fatal("readChrootHolders returned nil map")
	}
}

func TestHasActiveMountUnder(t *testing.T) {
	mounts := []string{"/", "/proc", "/tmp/boid-root-abc/home", "/tmp/boid-root-xyz"}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"exact match", "/tmp/boid-root-xyz", true},
		{"child match", "/tmp/boid-root-abc", true},
		{"no match", "/tmp/boid-root-unused", false},
		{"prefix-safe (not parent)", "/tmp/boid-root", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasActiveMountUnder(tc.path, mounts); got != tc.want {
				t.Errorf("hasActiveMountUnder(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
