package dispatcher_test

import (
	"context"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// capturingLaunchBackend is a minimal backend.SandboxBackend that records
// every LaunchOptions it receives and hands back a fixed-ID session — just
// enough for Runner.Dispatch's post-Launch bookkeeping (job.RuntimeID =
// session.ID()) to succeed, without a real container/userns runtime.
type capturingLaunchBackend struct {
	launches []backend.LaunchOptions
}

var _ backend.SandboxBackend = (*capturingLaunchBackend)(nil)

func (b *capturingLaunchBackend) Launch(_ context.Context, _ sandbox.Spec, opts backend.LaunchOptions) (backend.SandboxSession, error) {
	b.launches = append(b.launches, opts)
	return &capturingLaunchSession{id: "fake-runtime-" + opts.JobID}, nil
}

func (b *capturingLaunchBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}

func (b *capturingLaunchBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

type capturingLaunchSession struct{ id string }

var _ backend.SandboxSession = (*capturingLaunchSession)(nil)

func (s *capturingLaunchSession) ID() string { return s.id }
func (s *capturingLaunchSession) Subscribe() ([]byte, <-chan []byte, func(), bool) {
	return nil, nil, func() {}, false
}
func (s *capturingLaunchSession) WriteInput([]byte) error { return nil }
func (s *capturingLaunchSession) CloseInput() error       { return nil }
func (s *capturingLaunchSession) Resize(backend.TerminalSize) error {
	return nil
}
func (s *capturingLaunchSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return backend.RuntimeExit{}, nil
}
func (s *capturingLaunchSession) Stop(context.Context) error { return nil }
func (s *capturingLaunchSession) Signal(context.Context, syscall.Signal) error {
	return nil
}

// TestDispatch_LaunchOptions_CarriesWorkspaceAndDockerEnabled is the PR9
// regression guard for a confirmed wiring gap (docs/plans/
// phase6-container-backend.md §PR9): Runner.launchSandbox's
// backend.LaunchOptions{} literal never set Workspace or DockerEnabled at
// its one real call site (Dispatch), even though both values were already
// resolved and in scope there (workspaceID / spec.Visibility.DockerEnabled)
// — silently leaving containerBackend.Launch's opts.Workspace/DockerEnabled
// at their zero value on every production dispatch, regardless of the
// job's actual workspace or capabilities.docker declaration. The userns
// backend never reads either field, so nothing exercised this gap before
// PR9's container e2e work needed it. This test dispatches directly (not
// through the container backend) against a capturing fake, so it pins the
// wiring itself independent of any real docker/container-backend
// machinery.
func TestDispatch_LaunchOptions_CarriesWorkspaceAndDockerEnabled(t *testing.T) {
	r, d := newDispatchRunner(t)
	be := &capturingLaunchBackend{}
	r.Backend = be
	r.Sandbox = newFakeSandboxPrep(t)
	r.Runtime = newStatefulRuntime()
	r.Projects = orchestrator.DBProjectCatalog{DB: d.Conn}
	r.RuntimesDir = t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID:      "proj-ws",
		WorkDir: "/tmp",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// WorkspaceID is a separate project_workspaces join table, not a column
	// on projects itself — CreateProject's own INSERT statement never
	// touches it (see project_catalog.go's GetProject LEFT JOIN); it must
	// be set via SetProjectWorkspace for GetProject to read it back.
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-ws", "my-workspace"); err != nil {
		t.Fatalf("set project workspace: %v", err)
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-ws",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			DockerEnabled: true,
		},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected non-empty job ID")
	}

	if len(be.launches) != 1 {
		t.Fatalf("Launch calls = %d, want 1", len(be.launches))
	}
	got := be.launches[0]
	if got.Workspace != "my-workspace" {
		t.Errorf("LaunchOptions.Workspace = %q, want %q", got.Workspace, "my-workspace")
	}
	if !got.DockerEnabled {
		t.Error("LaunchOptions.DockerEnabled = false, want true (spec.Visibility.DockerEnabled was set)")
	}
}

// TestDispatch_LaunchOptions_DockerDisabled_LeavesDockerEnabledFalse is the
// negative-case pin: a job that does NOT declare capabilities.docker must
// see DockerEnabled=false, not just "whatever the last dispatch happened to
// set" — guards against a wiring fix that hardcodes true instead of
// actually threading spec.Visibility.DockerEnabled through.
func TestDispatch_LaunchOptions_DockerDisabled_LeavesDockerEnabledFalse(t *testing.T) {
	r, d := newDispatchRunner(t)
	be := &capturingLaunchBackend{}
	r.Backend = be
	r.Sandbox = newFakeSandboxPrep(t)
	r.Runtime = newStatefulRuntime()
	r.Projects = orchestrator.DBProjectCatalog{DB: d.Conn}
	r.RuntimesDir = t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID:      "proj-nodoc",
		WorkDir: "/tmp",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-nodoc", "another-workspace"); err != nil {
		t.Fatalf("set project workspace: %v", err)
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-nodoc",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(be.launches) != 1 {
		t.Fatalf("Launch calls = %d, want 1", len(be.launches))
	}
	got := be.launches[0]
	if got.Workspace != "another-workspace" {
		t.Errorf("LaunchOptions.Workspace = %q, want %q", got.Workspace, "another-workspace")
	}
	if got.DockerEnabled {
		t.Error("LaunchOptions.DockerEnabled = true, want false (job declared no docker capability)")
	}
}
