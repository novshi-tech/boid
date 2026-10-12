package orchestrator_test

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file pins PR4 of docs/plans/workspace-default-project.md:
// ProjectStore.GetWithWorkspace's merge of the workspace's default project
// definition (task_behaviors / base_branch / fork_point /
// default_task_behavior) into the hydrated meta, per 決定1 (field-by-field,
// project.yaml wins), 決定4 (task_behaviors merges by canonical name, same
// name = project.yaml wins outright), and 決定5 (empty = unspecified =
// inherit).

// writeProjectYAML (spec_loader_test.go) writes an arbitrary project.yaml
// body — reused here for tests that need task_behaviors/base_branch content
// setupProjectDir/setupProjectWithBehavior (project_store_test.go /
// project_store_hydrate_test.go) don't parameterize.

// TestGetWithWorkspace_TaskBehaviors_WorkspaceSuppliedBehaviorGetsHostCommandsAndEnvInjected
// pins the ORDERING fix documented in GetWithWorkspace's own comment: the
// task_behaviors merge must run BEFORE the host_commands/env blocks, or a
// workspace-default-supplied behavior (which did not exist yet when those
// blocks originally ran) would miss the same per-behavior HostCommands/Env
// injection a project.yaml-defined behavior gets.
func TestGetWithWorkspace_TaskBehaviors_WorkspaceSuppliedBehaviorGetsHostCommandsAndEnvInjected(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, "id: proj-inject\nname: Inject Project\n")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "inject-ws", `host_commands:
  - gh
env:
  WS_VAR: from-workspace
task_behaviors:
  release:
    traits: [ship]
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	s.SetHostCommands(map[string]orchestrator.HostCommandSpec{
		"gh": {Allow: []string{"pr"}},
	})
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-inject", WorkDir: projectDir, WorkspaceID: "inject-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-inject")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	release, ok := meta.TaskBehaviors["release"]
	if !ok {
		t.Fatalf("TaskBehaviors missing \"release\": %+v", meta.TaskBehaviors)
	}
	if _, ok := release.HostCommands["gh"]; !ok {
		t.Errorf("release.HostCommands = %+v, want \"gh\" injected (workspace-supplied behaviors must get the same per-behavior host_commands merge project.yaml-defined ones do)", release.HostCommands)
	}
	if release.Env["WS_VAR"] != "from-workspace" {
		t.Errorf("release.Env[WS_VAR] = %q, want from-workspace", release.Env["WS_VAR"])
	}
}

// TestGetWithWorkspace_BaseBranchForkPointDefaultTaskBehavior_InheritedWhenProjectEmpty
// pins 決定5: project.yaml leaves all 3 fields unset, so the workspace
// default's values are inherited.
func TestGetWithWorkspace_BaseBranchForkPointDefaultTaskBehavior_InheritedWhenProjectEmpty(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, "id: proj-inherit\nname: Inherit Project\n")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "inherit-ws", `base_branch: main
fork_point: origin/main
default_task_behavior: executor
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-inherit", WorkDir: projectDir, WorkspaceID: "inherit-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-inherit")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if meta.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want inherited main", meta.BaseBranch)
	}
	if meta.ForkPoint != "origin/main" {
		t.Errorf("ForkPoint = %q, want inherited origin/main", meta.ForkPoint)
	}
	if meta.DefaultTaskBehavior != "executor" {
		t.Errorf("DefaultTaskBehavior = %q, want inherited executor", meta.DefaultTaskBehavior)
	}
}

// TestGetWithWorkspace_DefaultProjectFields_DegradedWindowNotApplied pins
// the same degraded-window contract every other workspace-level field
// already has (TestGetWithWorkspace_Degraded): when workspace.yaml is
// missing, only SecretNamespace is injected — the workspace default project
// definition must NOT apply either, same as capabilities/host_commands/env.
func TestGetWithWorkspace_DefaultProjectFields_DegradedWindowNotApplied(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, "id: proj-degraded-default\nname: Degraded Default Project\n")

	wsDir := t.TempDir() // no workspace.yaml written here — degraded window

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-degraded-default", WorkDir: projectDir, WorkspaceID: "degraded-default-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-degraded-default")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if meta.SecretNamespace != "degraded-default-ws" {
		t.Errorf("SecretNamespace = %q, want degraded-default-ws (still injected in the degraded window)", meta.SecretNamespace)
	}
	if len(meta.TaskBehaviors) != 1 || meta.TaskBehaviors["judge"].Readonly == nil {
		t.Errorf("TaskBehaviors = %+v, want empty (no workspace default applied in the degraded window)", meta.TaskBehaviors)
	}
	if meta.BaseBranch != "" {
		t.Errorf("BaseBranch = %q, want empty", meta.BaseBranch)
	}
}
