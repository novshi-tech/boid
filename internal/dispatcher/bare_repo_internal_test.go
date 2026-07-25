package dispatcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRefreshBareRepoHEAD_SymbolicRefFailure_NonFatal pins Minor 3 (PR-2a
// codex round-2 review): refreshBareRepoHEAD's own doc comment explicitly
// promises every step is best-effort/non-fatal ("the fetch itself already
// succeeded ... never a regression when it can't") — but the FINAL
// `git symbolic-ref HEAD <ref>` call used to return its error directly
// instead of swallowing it like the two steps before it, silently
// contradicting that doc comment: a bare repo whose HEAD happened to be
// unwritable at just the wrong moment reported the WHOLE
// `boid project fetch` as failed even though every ref had already fetched
// successfully.
//
// This is a white-box (package dispatcher, not dispatcher_test) test
// because refreshBareRepoHEAD is unexported — going through the full
// exported FetchBareRepo instead would risk the SAME permission failure
// also breaking the `git fetch --all --prune` step it depends on (which
// writes into the bare repo's top-level directory too), muddying which
// step actually failed. Calling refreshBareRepoHEAD directly isolates the
// symbolic-ref failure precisely.
func TestRefreshBareRepoHEAD_SymbolicRefFailure_NonFatal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("permission-bit test assumes POSIX permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	src := t.TempDir()
	runGitForTest(t, src, "init", "-q", "-b", "main")
	runGitForTest(t, src, "config", "user.email", "test@example.com")
	runGitForTest(t, src, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitForTest(t, src, "add", ".")
	runGitForTest(t, src, "commit", "-q", "-m", "initial")

	dest := filepath.Join(t.TempDir(), "dest.git")
	if err := CloneBareRepo(context.Background(), src, dest, nil, "default"); err != nil {
		t.Fatalf("CloneBareRepo: %v", err)
	}

	// Make the bare repo's top-level directory read-only so `git
	// symbolic-ref HEAD <ref>` (which rewrites HEAD via a lock-file rename
	// directly inside dest) cannot write. `git ls-remote --symref origin
	// HEAD` (a pure read, querying src directly) still succeeds under
	// 0o500 (read+execute), so this isolates the refresh step's OWN
	// failure without touching the ls-remote step before it.
	if err := os.Chmod(dest, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dest, 0o755) })

	if err := refreshBareRepoHEAD(context.Background(), dest, nil); err != nil {
		t.Errorf("refreshBareRepoHEAD = %v, want nil (best-effort per its own doc comment)", err)
	}
}
