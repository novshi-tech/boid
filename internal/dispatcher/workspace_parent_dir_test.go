package dispatcher_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"github.com/novshi-tech/boid/testutil"
)

// capturingSandboxBackend is a minimal backend.SandboxBackend that records
// the last sandbox.Spec passed to Launch, so Dispatch-level tests can
// assert on the fully-resolved mounts/WorkDir/Clone fields — the same shape
// BuildSandboxSpec produces internally, but reachable only through the real
// Dispatch() call path (see .claude/skills/boid-review's wiring-seam
// doctrine: a unit test of BuildSandboxSpec alone would not catch a dropped
// Runner.Dispatch wiring step).
//
// Replaces the pre-PR-4 capturingSandboxPrep (a SandboxPreparer stub —
// docs/plans/volume-only-daemon.md §論点e removed that userns-only seam);
// Launch's own spec parameter already carries the exact same sandbox.Spec a
// SandboxPreparer used to receive, so this simplifies to recording it
// directly. Reuses capturingLaunchSession (runner_launch_options_workspace_test.go,
// same package) for the returned session rather than defining a second,
// near-identical stub.
type capturingSandboxBackend struct {
	spec sandbox.Spec
}

var _ backend.SandboxBackend = (*capturingSandboxBackend)(nil)

func (b *capturingSandboxBackend) Launch(_ context.Context, spec sandbox.Spec, opts backend.LaunchOptions) (backend.SandboxSession, error) {
	b.spec = spec
	return &capturingLaunchSession{id: "fake-runtime-" + opts.JobID}, nil
}

func (b *capturingSandboxBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}

// RunWorkspaceInit satisfies dispatcher.WorkspaceInitExecutor, which
// resolveWorkspaceHome requires of every backend since PR5 — see
// runWorkspaceInitInProcess (runtime_test_helpers_test.go).
func (b *capturingSandboxBackend) EnsureWorkspaceHomeVolume(_ context.Context, req dispatcher.WorkspaceHomeVolumeRequest) (string, error) {
	return ensureWorkspaceHomeVolumeInProcess(req)
}

func (b *capturingSandboxBackend) RunWorkspaceInit(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	return runWorkspaceInitInProcess(ctx, req)
}

func (b *capturingSandboxBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// findMountTarget returns the first mount in mounts whose Target matches, or
// nil.
func findMountTarget(mounts []sandbox.Mount, target string) *sandbox.Mount {
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

func TestDispatch_CloneMode_NameScopedWorkspaceDir(t *testing.T) {
	bin := t.TempDir()
	gitCalls := filepath.Join(bin, "git-calls")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf x >> \"$GIT_CALLS\"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CALLS", gitCalls)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/host/bm-next", UpstreamURL: "https://github.com/owner/bm-next.git",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gwURL := "http://10.0.2.2:9"
	prep := &capturingSandboxBackend{}
	r := &dispatcher.Runner{
		DB:          d.Conn,
		Projects:    orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:     prep,
		BoidBinary:  "/boid",
		GitGateway:  gitgateway.NewRegistry(),
		GatewayURL:  &gwURL,
		RuntimesDir: t.TempDir(),
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			ProjectDir:  "/host/bm-next",
			ProjectName: "bm-next",
			Writable:    true,
			Checkout:    true,
		},
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	const wantDir = "/workspace/bm-next"
	if prep.spec.WorkDir != wantDir {
		t.Errorf("WorkDir = %q, want %q", prep.spec.WorkDir, wantDir)
	}
	if _, err := os.Stat(gitCalls); !os.IsNotExist(err) {
		t.Fatalf("daemon invoked git: %v", err)
	}
	if prep.spec.Env["BOID_PRIMARY_REPO"] != "github.com/owner/bm-next" {
		t.Fatalf("checkout identity = %q", prep.spec.Env["BOID_PRIMARY_REPO"])
	}
	for _, m := range prep.spec.Mounts {
		if strings.HasPrefix(m.Target, "/mnt/refs") || strings.HasPrefix(m.Target, "/workspace/") {
			t.Errorf("unexpected repository mount: %+v", m)
		}
	}
}

// TestDispatch_CloneMode_FallsBackToProjectDirBasenameWhenNameUnset pins the
// fallback half of the same decision: a project dispatched with no
// ProjectName set (e.g. project.yaml has no `name:` field) still lands at a
// distinct, deterministic directory instead of colliding on the bare
// "/workspace" parent.
func TestDispatch_CloneMode_FallsBackToProjectDirBasenameWhenNameUnset(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/host/sumiron-project", UpstreamURL: "https://github.com/owner/sumiron-project.git",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gwURL := "http://10.0.2.2:9"
	prep := &capturingSandboxBackend{}
	r := &dispatcher.Runner{
		DB:          d.Conn,
		Projects:    orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:     prep,
		BoidBinary:  "/boid",
		GitGateway:  gitgateway.NewRegistry(),
		GatewayURL:  &gwURL,
		RuntimesDir: t.TempDir(),
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			ProjectDir: "/host/sumiron-project", // no ProjectName
			Writable:   true,
			Checkout:   true,
		},
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	const wantDir = "/workspace/sumiron-project"
	if prep.spec.WorkDir != wantDir {
		t.Errorf("WorkDir = %q, want %q", prep.spec.WorkDir, wantDir)
	}
}
