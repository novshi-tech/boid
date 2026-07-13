package dispatcher_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/testutil"
)

// capturingSandboxPrep is a SandboxPreparer stub that records the last
// sandbox.Spec it was asked to prepare, so Dispatch-level tests can assert on
// the fully-resolved mounts/WorkDir/Clone fields — the same shape BuildSandboxSpec
// produces internally, but reachable only through the real Dispatch() call path
// (see .claude/skills/boid-review's wiring-seam doctrine: a unit test of
// BuildSandboxSpec alone would not catch a dropped Runner.Dispatch wiring step).
type capturingSandboxPrep struct {
	dir  string
	spec sandbox.Spec
}

func (p *capturingSandboxPrep) PrepareSandbox(spec sandbox.Spec) (*dispatcher.PreparedSandbox, error) {
	p.spec = spec
	specPath := filepath.Join(p.dir, "runner-spec.json")
	if err := os.WriteFile(specPath, []byte("{}"), 0o600); err != nil {
		return nil, fmt.Errorf("write runner spec: %w", err)
	}
	return &dispatcher.PreparedSandbox{
		SpecPath:  specPath,
		StatePath: filepath.Join(p.dir, "runner-state.json"),
	}, nil
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

// TestDispatch_CloneMode_NameScopedWorkspaceDir is the end-to-end regression
// guard for the workspace 親化リファクタリング (nose 2026-07-13 decision): a
// clone-mode dispatch for a project with project.yaml's `name: bm-next` must
// land the sandbox's clone mount, WorkDir, and CloneSpec.TargetDir all at
// "/workspace/bm-next" — not the flat "/workspace" every project used to
// share (the root cause of the Claude Code `~/.claude/projects/-workspace/`
// session-log collision this refactor exists to fix).
//
// spec.Visibility.ProjectName is set directly here, mirroring what
// orchestrator.PlanHook / dispatcher.BuildSessionJobSpec already do at
// JobSpec-build time in production (see orchestrator.Visibility.ProjectName's
// doc comment) — Runner.Dispatch itself never re-derives the name from a
// Projects lookup.
func TestDispatch_CloneMode_NameScopedWorkspaceDir(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/host/bm-next", UpstreamURL: "https://github.com/owner/bm-next.git",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gwURL := "http://10.0.2.2:9"
	prep := &capturingSandboxPrep{dir: t.TempDir()}
	r := &dispatcher.Runner{
		DB:          d.Conn,
		Projects:    orchestrator.DBProjectCatalog{DB: d.Conn},
		Sandbox:     prep,
		Runtime:     newStatefulRuntime(),
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
			Clone:       &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	const wantDir = "/workspace/bm-next"
	if prep.spec.WorkDir != wantDir {
		t.Errorf("WorkDir = %q, want %q", prep.spec.WorkDir, wantDir)
	}
	if !prep.spec.Clone.Enabled {
		t.Fatal("Clone.Enabled = false, want true")
	}
	if prep.spec.Clone.TargetDir != wantDir {
		t.Errorf("Clone.TargetDir = %q, want %q", prep.spec.Clone.TargetDir, wantDir)
	}
	if m := findMountTarget(prep.spec.Mounts, wantDir); m == nil {
		t.Errorf("no mount with Target %q found among %#v", wantDir, prep.spec.Mounts)
	}
	if m := findMountTarget(prep.spec.Mounts, "/workspace"); m != nil {
		t.Errorf("unexpected bare /workspace mount (should be name-scoped): %+v", m)
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
	prep := &capturingSandboxPrep{dir: t.TempDir()}
	r := &dispatcher.Runner{
		DB:          d.Conn,
		Projects:    orchestrator.DBProjectCatalog{DB: d.Conn},
		Sandbox:     prep,
		Runtime:     newStatefulRuntime(),
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
			Clone:      &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
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
