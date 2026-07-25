package dispatcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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

// newPerJobCloneRunner builds a Runner wired identically to
// TestDispatch_ContainerBackend_BareRepoProject_PreClonesIntoStagingDir,
// factored out so the WithProjectLock tests below (PR834 PR-2b round-2
// codex review Major 1) don't have to repeat the container-backend
// per-job-clone wiring.
func newPerJobCloneRunner(t *testing.T, bareRepoPath, runtimesDir string) *Runner {
	t.Helper()
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: bareRepoPath}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})
	return &Runner{
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
}

func perJobCloneSpec(bareRepoPath string) *orchestrator.JobSpec {
	return &orchestrator.JobSpec{
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
}

// TestDispatch_ContainerBackend_WithProjectLockWrapsFetchAndClone pins
// PR834 PR-2b round-2 codex review Major 1's fix: the per-job-clone
// dispatch step (FetchBareRepo + PrepareJobCheckout against
// selfProject.WorkDir) must run INSIDE r.WithProjectLock when it is wired,
// not alongside or after it — the whole point of the fix is that a
// concurrent `project rm` + re-add at the same managed path cannot land
// between the fetch and the clone. Proven here by a fake WithProjectLock
// that only calls fn if invoked, and asserting the staging dir was
// actually populated (i.e. fn — the real fetch+clone closure — really ran
// through the wrapper) exactly once.
func TestDispatch_ContainerBackend_WithProjectLockWrapsFetchAndClone(t *testing.T) {
	bareRepoPath := setupPerJobCloneBareRepo(t)
	runtimesDir := t.TempDir()
	r := newPerJobCloneRunner(t, bareRepoPath, runtimesDir)

	calls := 0
	r.WithProjectLock = func(fn func() error) error {
		calls++
		return fn()
	}

	jobID, err := r.Dispatch(context.Background(), perJobCloneSpec(bareRepoPath), nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("WithProjectLock call count = %d, want 1", calls)
	}

	stagingDir := filepath.Join(runtimesDir, jobID, "workspace")
	if _, err := os.ReadFile(filepath.Join(stagingDir, "README.md")); err != nil {
		t.Fatalf("expected the fetch+clone closure to have run through WithProjectLock and populated %s: %v", stagingDir, err)
	}
}

// TestDispatch_ContainerBackend_WithProjectLockError_FailsJobWithoutCloning
// pins error propagation: when WithProjectLock itself fails WITHOUT ever
// calling fn (e.g. a hypothetical future timeout/contention error from the
// lock helper), Dispatch must fail the job with that error, never populate
// the staging dir, and never track a checkoutDirs entry for it — the exact
// same "never left half-registered" contract PrepareJobCheckout's own
// failure paths already guarantee (checkout_test.go's CleansUpStagingDir
// tests), now also holding for a failure that occurs BEFORE the fetch/clone
// closure ever runs.
func TestDispatch_ContainerBackend_WithProjectLockError_FailsJobWithoutCloning(t *testing.T) {
	bareRepoPath := setupPerJobCloneBareRepo(t)
	runtimesDir := t.TempDir()
	r := newPerJobCloneRunner(t, bareRepoPath, runtimesDir)

	wantErr := errors.New("project lock unavailable")
	r.WithProjectLock = func(fn func() error) error {
		return wantErr
	}

	jobID, err := r.Dispatch(context.Background(), perJobCloneSpec(bareRepoPath), nil)
	if err == nil {
		t.Fatal("expected Dispatch to fail when WithProjectLock itself errors")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Dispatch error = %v, want it to wrap %v", err, wantErr)
	}

	stagingDir := filepath.Join(runtimesDir, jobID, "workspace")
	if _, statErr := os.Stat(stagingDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected no staging dir when WithProjectLock never ran the clone, stat err = %v", statErr)
	}
	r.checkoutMu.Lock()
	_, tracked := r.checkoutDirs[jobID]
	r.checkoutMu.Unlock()
	if tracked {
		t.Fatal("checkoutDirs should not track a job whose WithProjectLock call failed")
	}
}

// TestDispatch_ContainerBackend_WithProjectLockSerializesConcurrentDispatches
// is the concurrency-shaped sibling of the two tests above: with a REAL
// mutex-backed WithProjectLock (the shape wire.go actually wires —
// api.ProjectAppService.WithProjectLock holds a real sync.Mutex around fn),
// two Dispatch calls racing against the same project must never run their
// fetch+clone closures concurrently. This is the dispatcher-package-side
// half of the fix; the other half (that WithProjectLock's underlying
// projectMu is the SAME lock CreateProject/CreateProjectFromGitURL/
// DeleteProject/FetchProject already hold) is pinned by
// internal/api/project_service_test.go's own
// TestProjectAppService_WithProjectLock_SerializesConcurrentCallers — the
// two together cover the full cross-package serialization contract without
// internal/dispatcher needing to import internal/api (which would be an
// import cycle — see Runner.WithProjectLock's own doc comment).
func TestDispatch_ContainerBackend_WithProjectLockSerializesConcurrentDispatches(t *testing.T) {
	bareRepoPath := setupPerJobCloneBareRepo(t)
	runtimesDir := t.TempDir()
	r := newPerJobCloneRunner(t, bareRepoPath, runtimesDir)

	var realMu sync.Mutex
	var stateMu sync.Mutex
	active, maxActive := 0, 0
	r.WithProjectLock = func(fn func() error) error {
		realMu.Lock()
		defer realMu.Unlock()
		stateMu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		stateMu.Unlock()
		err := fn()
		stateMu.Lock()
		active--
		stateMu.Unlock()
		return err
	}

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.Dispatch(context.Background(), perJobCloneSpec(bareRepoPath), nil)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Dispatch[%d]: %v", i, err)
		}
	}
	if maxActive != 1 {
		t.Fatalf("max concurrently-active WithProjectLock fetch+clone closures = %d, want 1 (not serialized)", maxActive)
	}
}
