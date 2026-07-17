package humanize

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestFormatBytes pins the SI (1000-based) formatting contract documented on
// FormatBytes: plain "%d B" below 1000, "%.2f <unit>" (2 decimal places)
// above it, and the KB/MB/GB/TB/PB unit boundaries.
func TestFormatBytes(t *testing.T) {
	cases := []struct {
		name  string
		bytes int64
		want  string
	}{
		{"zero", 0, "0 B"},
		{"small", 500, "500 B"},
		{"just_under_1kb", 999, "999 B"},
		{"exactly_1kb", 1000, "1.00 KB"},
		{"1.5kb", 1500, "1.50 KB"},
		{"just_under_1mb", 999_999, "1000.00 KB"},
		{"1.23mb", 1_234_567, "1.23 MB"},
		{"1.23gb", 1_230_000_000, "1.23 GB"},
		{"1tb", 1_000_000_000_000, "1.00 TB"},
		{"999tb", 999_000_000_000_000, "999.00 TB"},
		{"1pb", 1_000_000_000_000_000, "1.00 PB"},
		{"huge", 2_500_000_000_000_000, "2.50 PB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatBytes(tc.bytes); got != tc.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
			}
		})
	}
}

// TestDirSize_EmptyDir pins the base case: an empty directory has size 0.
func TestDirSize_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := DirSize(dir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	if got != 0 {
		t.Errorf("DirSize(empty dir) = %d, want 0", got)
	}
}

// TestDirSize_SumsNestedFiles pins the recursive-sum contract: files in
// subdirectories are included, and directory entries themselves are not
// counted toward the total (only regular file content).
func TestDirSize_SumsNestedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), 100)
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	writeFile(t, filepath.Join(sub, "b.txt"), 250)
	writeFile(t, filepath.Join(sub, "c.txt"), 7)

	got, err := DirSize(dir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	if want := int64(100 + 250 + 7); got != want {
		t.Errorf("DirSize = %d, want %d", got, want)
	}
}

// TestDirSize_NonexistentPath_ReturnsError pins the error contract callers
// rely on to render "?" instead of a bogus size: a path that does not exist
// on disk must return a non-nil error.
func TestDirSize_NonexistentPath_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := DirSize(filepath.Join(dir, "does-not-exist"))
	if err == nil {
		t.Fatal("expected an error for a nonexistent path, got nil")
	}
}

// TestDirSize_UnreadableSubdir_ReturnsError pins the "permission denied
// partway through the walk" case (docs/plans/home-workspace-volume.md PR5
// test plan: "サイズ計算エラー (dir を読めない symlink 等) → '?'"). Skipped
// when running as root, since root ignores directory permission bits and
// the walk would succeed instead of erroring.
func TestDirSize_UnreadableSubdir_ReturnsError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("permission-bit test assumes POSIX permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "visible.txt"), 10)
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	writeFile(t, filepath.Join(locked, "secret.txt"), 20)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod locked: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) // let t.TempDir() clean up.

	_, err := DirSize(dir)
	if err == nil {
		t.Fatal("expected an error walking into a permission-denied subdirectory, got nil")
	}
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
