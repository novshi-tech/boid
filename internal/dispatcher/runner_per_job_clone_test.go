package dispatcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file is the Dispatch-level wiring-seam guard for docs/plans/
// volume-only-daemon.md §論点b's "per-job clone at dispatch time" (PR-2b):
// it proves Runner.Dispatch itself reaches PrepareJobCheckout/
// CleanupJobCheckout for a git-URL-registered (bare-repo-backed) project
// dispatched under the container backend — not just that those primitives
// work in isolation (internal/dispatcher/checkout_test.go already pins
// that) — mirroring gitgateway_wire_test.go's own "Dispatch-level guard"
// doctrine for the identical class of gap (a dropped call site between two
// otherwise-correct halves of a wiring seam).

func runGitPJC(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setupPerJobCloneBareRepo builds a plain source repo with a commit on
// "main", then mirror-clones it into a bare repo (the exact shape §論点b's
// layout / orchestrator.IsBareRepoDir expects — HEAD file + objects/ dir
// directly under the repo root, not a .git subdirectory).
func setupPerJobCloneBareRepo(t *testing.T) (bareRepoPath string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	src := t.TempDir()
	runGitPJC(t, src, "init", "-q", "-b", "main")
	runGitPJC(t, src, "config", "user.email", "test@example.com")
	runGitPJC(t, src, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("per-job-clone fixture\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitPJC(t, src, "add", ".")
	runGitPJC(t, src, "commit", "-q", "-m", "initial")

	bareRepoPath = filepath.Join(t.TempDir(), "proj.git")
	runGitPJC(t, ".", "clone", "-q", "--mirror", src, bareRepoPath)
	return bareRepoPath
}

// TestDispatch_ContainerBackend_BareRepoProject_PreClonesIntoStagingDir
// pins the happy path: a git-URL-registered project (orchestrator.
// IsBareRepoDir(proj.WorkDir), PR-2a) dispatched under the container
// backend with Visibility.Clone set gets its cloneWorkspaceDir populated
// by Runner.Dispatch itself, via PrepareJobCheckout, BEFORE the job
// container ever starts — not left as an empty scratch dir for the
// sandbox's own in-container clone sequence.
func TestDispatch_ContainerBackend_BareRepoProject_PreClonesIntoStagingDir(t *testing.T) {
	bareRepoPath := setupPerJobCloneBareRepo(t)

	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: bareRepoPath}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	runtimesDir := t.TempDir()

	r := &Runner{
		DB:          d.Conn,
		Backend:     be,
		Sandbox:     &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:     &gwFakeRuntime{},
		BoidBinary:  "/boid",
		RuntimesDir: runtimesDir,
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: bareRepoPath, UpstreamURL: "https://example.com/owner/proj-1.git"},
		}},
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			ProjectDir:  bareRepoPath,
			ProjectName: "proj-1",
			Writable:    true,
			Clone:       &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	stagingDir := filepath.Join(runtimesDir, jobID, "workspace")
	data, rerr := os.ReadFile(filepath.Join(stagingDir, "README.md"))
	if rerr != nil {
		t.Fatalf("expected Dispatch to have pre-cloned the bare repo into %s, but reading README.md failed: %v", stagingDir, rerr)
	}
	if string(data) != "per-job-clone fixture\n" {
		t.Fatalf("README.md content = %q, want %q", data, "per-job-clone fixture\n")
	}

	r.checkoutMu.Lock()
	tracked, ok := r.checkoutDirs[jobID]
	r.checkoutMu.Unlock()
	if !ok || tracked != stagingDir {
		t.Fatalf("checkoutDirs[%q] = %q, %v; want %q, true", jobID, tracked, ok, stagingDir)
	}

	// UnregisterJob (job-completion cleanup) must remove the staging dir
	// (docs/plans/volume-only-daemon.md §論点b step 5) — the bare repo
	// cache itself is untouched (CleanupJobCheckout never removes it).
	r.UnregisterJob(jobID)
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Fatalf("expected staging dir %s to be removed after UnregisterJob, stat err = %v", stagingDir, err)
	}
	if _, err := os.Stat(bareRepoPath); err != nil {
		t.Fatalf("bare repo cache should survive staging-dir cleanup: %v", err)
	}
	r.checkoutMu.Lock()
	_, stillTracked := r.checkoutDirs[jobID]
	r.checkoutMu.Unlock()
	if stillTracked {
		t.Fatal("checkoutDirs entry should be removed after UnregisterJob")
	}
}

// TestDispatch_UsernsBackend_BareRepoProject_DoesNotPreClone pins the
// negative case: the userns backend never triggers the per-job-clone path
// (IsContainerBackend(r.Backend) is false for it) — cloneWorkspaceDir is
// left for the pre-existing in-sandbox clone sequence to populate exactly
// as before this PR, even for a git-URL-registered project.
func TestDispatch_UsernsBackend_BareRepoProject_DoesNotPreClone(t *testing.T) {
	bareRepoPath := setupPerJobCloneBareRepo(t)

	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: bareRepoPath}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	runtimesDir := t.TempDir()
	r := &Runner{
		DB:          d.Conn,
		Sandbox:     &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:     &gwFakeRuntime{},
		BoidBinary:  "/boid",
		RuntimesDir: runtimesDir,
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: bareRepoPath, UpstreamURL: "https://example.com/owner/proj-1.git"},
		}},
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			ProjectDir:  bareRepoPath,
			ProjectName: "proj-1",
			Writable:    true,
			Clone:       &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	stagingDir := filepath.Join(runtimesDir, jobID, "workspace")
	if _, err := os.ReadFile(filepath.Join(stagingDir, "README.md")); err == nil {
		t.Fatalf("expected the userns backend to NOT pre-clone (no PrepareJobCheckout call), but %s/README.md exists", stagingDir)
	}

	r.checkoutMu.Lock()
	_, tracked := r.checkoutDirs[jobID]
	r.checkoutMu.Unlock()
	if tracked {
		t.Fatal("checkoutDirs should not track a job the userns backend dispatched")
	}
}
