package api

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newTestRuntimesDir returns a runtimesDir (as server/wire.go's
// runtimesDirFor(cfg) would produce) whose sibling "homes" directory lives
// under t.TempDir() — matching dispatcher.WorkspaceHomesDir's derivation
// (filepath.Dir(runtimesDir)/homes).
func newTestRuntimesDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "runtimes")
}

func writeHomeFile(t *testing.T, runtimesDir, slug, name string, size int) {
	t.Helper()
	path, err := resolveWorkspaceHomePath(runtimesDir, slug)
	if err != nil {
		t.Fatalf("resolveWorkspaceHomePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir home dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, name), make([]byte, size), 0o644); err != nil {
		t.Fatalf("write home file: %v", err)
	}
}

// --- computeWorkspaceHomeSize ---

func TestComputeWorkspaceHomeSize_NotYetCreated(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)
	got := computeWorkspaceHomeSize(runtimesDir, "myws")
	if got.Exists {
		t.Errorf("Exists = true, want false for a home dir that was never created")
	}
	if got.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0", got.Bytes)
	}
	if got.SizeError != "" {
		t.Errorf("SizeError = %q, want empty (not-yet-created is not an error)", got.SizeError)
	}
	wantPath, _ := resolveWorkspaceHomePath(runtimesDir, "myws")
	if got.Path != wantPath {
		t.Errorf("Path = %q, want %q", got.Path, wantPath)
	}
}

func TestComputeWorkspaceHomeSize_ExistingContent(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)
	writeHomeFile(t, runtimesDir, "myws", "a.txt", 123)
	writeHomeFile(t, runtimesDir, "myws", "b.txt", 77)

	got := computeWorkspaceHomeSize(runtimesDir, "myws")
	if !got.Exists {
		t.Fatal("Exists = false, want true")
	}
	if got.SizeError != "" {
		t.Errorf("SizeError = %q, want empty", got.SizeError)
	}
	if got.Bytes != 200 {
		t.Errorf("Bytes = %d, want 200", got.Bytes)
	}
}

func TestComputeWorkspaceHomeSize_UnreadableDir_ReportsSizeError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("permission-bit test assumes POSIX permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}

	runtimesDir := newTestRuntimesDir(t)
	writeHomeFile(t, runtimesDir, "myws", "a.txt", 10)
	homePath, _ := resolveWorkspaceHomePath(runtimesDir, "myws")
	locked := filepath.Join(homePath, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod locked: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	got := computeWorkspaceHomeSize(runtimesDir, "myws")
	if !got.Exists {
		t.Error("Exists = false, want true (the home dir itself is readable, only a subdir is locked)")
	}
	if got.SizeError == "" {
		t.Error("SizeError = empty, want a non-empty error from the permission-denied subdirectory")
	}
	if got.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0 when SizeError is set", got.Bytes)
	}
}

// --- ListWorkspaceHomeSizes ---

type stubWorkspaceSlugLister struct {
	summaries []*orchestrator.WorkspaceSummary
	err       error
}

func (s *stubWorkspaceSlugLister) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	return s.summaries, s.err
}

func TestListWorkspaceHomeSizes_NoHomesDirYet(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)
	got, err := ListWorkspaceHomeSizes(runtimesDir, nil)
	if err != nil {
		t.Fatalf("ListWorkspaceHomeSizes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestListWorkspaceHomeSizes_ListsAllDirsAndFlagsOrphans(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)
	writeHomeFile(t, runtimesDir, "default", "x", 100)
	writeHomeFile(t, runtimesDir, "known-ws", "x", 200)
	writeHomeFile(t, runtimesDir, "orphan-ws", "x", 50)

	lister := &stubWorkspaceSlugLister{summaries: []*orchestrator.WorkspaceSummary{
		{ID: "default"},
		{ID: "known-ws"},
	}}

	got, err := ListWorkspaceHomeSizes(runtimesDir, lister)
	if err != nil {
		t.Fatalf("ListWorkspaceHomeSizes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3: %+v", len(got), got)
	}

	byslug := map[string]WorkspaceHomeSize{}
	var total int64
	for _, e := range got {
		byslug[e.Slug] = e
		total += e.Bytes
	}
	if total != 350 {
		t.Errorf("total bytes = %d, want 350", total)
	}
	if byslug["default"].Orphan {
		t.Error("default: Orphan = true, want false")
	}
	if byslug["known-ws"].Orphan {
		t.Error("known-ws: Orphan = true, want false")
	}
	if !byslug["orphan-ws"].Orphan {
		t.Error("orphan-ws: Orphan = false, want true (no matching workspace row)")
	}
}

// TestListWorkspaceHomeSizes_IgnoresMarkerAndLockFiles pins that the
// sibling homes/<slug>.init.json and homes/<slug>.lock files documented in
// docs/plans/home-workspace-volume.md's layout section are not mistaken for
// workspace home directories (os.ReadDir's IsDir() filter).
func TestListWorkspaceHomeSizes_IgnoresMarkerAndLockFiles(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)
	writeHomeFile(t, runtimesDir, "myws", "x", 10)
	// resolveWorkspaceHomePath(runtimesDir, "") joins the empty slug onto
	// homesDir, which filepath.Join collapses right back down to homesDir
	// itself (Join("/a/b", "") == "/a/b") — a convenient way to resolve
	// homesDir without duplicating dispatcher.WorkspaceHomesDir's own call.
	homesDir, err := resolveWorkspaceHomePath(runtimesDir, "")
	if err != nil {
		t.Fatalf("resolveWorkspaceHomePath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homesDir, "myws.init.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homesDir, "myws.lock"), nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	got, err := ListWorkspaceHomeSizes(runtimesDir, nil)
	if err != nil {
		t.Fatalf("ListWorkspaceHomeSizes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (marker/lock files must not be listed): %+v", len(got), got)
	}
	if got[0].Slug != "myws" {
		t.Errorf("Slug = %q, want myws", got[0].Slug)
	}
}

func TestListWorkspaceHomeSizes_ListerErrorDegradesGracefully(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)
	writeHomeFile(t, runtimesDir, "myws", "x", 10)

	lister := &stubWorkspaceSlugLister{err: errors.New("db unavailable")}
	got, err := ListWorkspaceHomeSizes(runtimesDir, lister)
	if err != nil {
		t.Fatalf("ListWorkspaceHomeSizes should not fail on a lister error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if !got[0].Orphan {
		t.Error("Orphan = false, want true (degrades to 'unknown' on lister failure)")
	}
}

// --- deleteWorkspaceHome ---

func TestDeleteWorkspaceHome_DefaultWorkspace_NeverDeleted(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)
	writeHomeFile(t, runtimesDir, orchestrator.DefaultWorkspaceSlug, "x", 10)

	info, deleted, err := deleteWorkspaceHome(runtimesDir, orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("deleteWorkspaceHome: %v", err)
	}
	if deleted {
		t.Error("deleted = true, want false (default workspace home must never be deleted)")
	}
	homePath, _ := resolveWorkspaceHomePath(runtimesDir, orchestrator.DefaultWorkspaceSlug)
	if _, statErr := os.Stat(homePath); statErr != nil {
		t.Errorf("default workspace home dir was removed from disk: stat err=%v", statErr)
	}
	if info.Path != homePath {
		t.Errorf("info.Path = %q, want %q", info.Path, homePath)
	}
}

func TestDeleteWorkspaceHome_ExistingHome_RemovesFromDisk(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)
	writeHomeFile(t, runtimesDir, "myws", "x", 42)
	homePath, _ := resolveWorkspaceHomePath(runtimesDir, "myws")

	info, deleted, err := deleteWorkspaceHome(runtimesDir, "myws")
	if err != nil {
		t.Fatalf("deleteWorkspaceHome: %v", err)
	}
	if !deleted {
		t.Error("deleted = false, want true")
	}
	if info.Bytes != 42 {
		t.Errorf("info.Bytes = %d, want 42 (size as observed before deletion)", info.Bytes)
	}
	if _, statErr := os.Stat(homePath); !os.IsNotExist(statErr) {
		t.Errorf("home dir still present after delete: stat err=%v", statErr)
	}
}

func TestDeleteWorkspaceHome_NotYetCreated_NoOp(t *testing.T) {
	runtimesDir := newTestRuntimesDir(t)

	info, deleted, err := deleteWorkspaceHome(runtimesDir, "myws")
	if err != nil {
		t.Fatalf("deleteWorkspaceHome: %v", err)
	}
	if deleted {
		t.Error("deleted = true, want false (nothing to delete)")
	}
	if info.Exists {
		t.Error("info.Exists = true, want false")
	}
}

func TestDeleteWorkspaceHome_PermissionDenied_ReturnsErrorNotDeleted(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("permission-bit test assumes POSIX permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}

	runtimesDir := newTestRuntimesDir(t)
	homePath, _ := resolveWorkspaceHomePath(runtimesDir, "myws")
	homesDir := filepath.Dir(homePath)
	if err := os.MkdirAll(homesDir, 0o755); err != nil {
		t.Fatalf("mkdir homes dir: %v", err)
	}
	if err := os.MkdirAll(homePath, 0o755); err != nil {
		t.Fatalf("mkdir home dir: %v", err)
	}
	// Deny read+execute on homes/ itself so even os.Stat(homePath) fails
	// with a permission error (not ENOENT) — exercising deleteWorkspaceHome's
	// "could not even size it, do not attempt a blind RemoveAll" branch.
	if err := os.Chmod(homesDir, 0o000); err != nil {
		t.Fatalf("chmod homes dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(homesDir, 0o755) })

	_, deleted, err := deleteWorkspaceHome(runtimesDir, "myws")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if deleted {
		t.Error("deleted = true, want false")
	}
}
