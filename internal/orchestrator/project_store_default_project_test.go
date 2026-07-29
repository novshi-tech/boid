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

// TestGetWithWorkspace_DefaultProjectFields_UnaffectedWhenWorkspaceEmpty is
// the doc's own PR4 requirement: "既存 project の挙動不変: workspace default
// が空のとき、hydrate の出力が改修前と一致すること". Every existing
// workspace has empty task_behaviors/base_branch/fork_point/
// default_task_behavior (PR3 only added the columns; PR4 is the first PR
// that reads them), so the merge blocks added in GetWithWorkspace must be
// no-ops for every one of them.
func TestGetWithWorkspace_DefaultProjectFields_UnaffectedWhenWorkspaceEmpty(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, `id: proj-noop
name: No-op Project
base_branch: feature/x
task_behaviors:
  executor:
    traits: [impl]
`)

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "empty-defaults-ws", "env:\n  UNRELATED: yes\n")

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-noop", WorkDir: projectDir, WorkspaceID: "empty-defaults-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-noop")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if meta.BaseBranch != "feature/x" {
		t.Errorf("BaseBranch = %q, want feature/x (project.yaml's own value untouched)", meta.BaseBranch)
	}
	if meta.ForkPoint != "" {
		t.Errorf("ForkPoint = %q, want empty", meta.ForkPoint)
	}
	if meta.DefaultTaskBehavior != "" {
		t.Errorf("DefaultTaskBehavior = %q, want empty", meta.DefaultTaskBehavior)
	}
	if len(meta.TaskBehaviors) != 2 { // "executor" + its "dev" alias mirror, same as pre-PR4
		t.Fatalf("TaskBehaviors = %+v, want exactly executor+dev (project.yaml's own, unaffected by an empty workspace default)", meta.TaskBehaviors)
	}
	if _, ok := meta.TaskBehaviors["executor"]; !ok {
		t.Errorf("TaskBehaviors missing executor: %+v", meta.TaskBehaviors)
	}
}

// TestGetWithWorkspace_TaskBehaviors_WorkspaceSuppliesMissingBehavior pins
// 決定4's core case: a canonical behavior name project.yaml does not define
// is filled in from the workspace default.
func TestGetWithWorkspace_TaskBehaviors_WorkspaceSuppliesMissingBehavior(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, `id: proj-fill
name: Fill Project
task_behaviors:
  executor:
    traits: [impl]
`)

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "fill-ws", `task_behaviors:
  release:
    traits: [ship]
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-fill", WorkDir: projectDir, WorkspaceID: "fill-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-fill")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	release, ok := meta.TaskBehaviors["release"]
	if !ok {
		t.Fatalf("TaskBehaviors = %+v, want a \"release\" entry supplied by the workspace default", meta.TaskBehaviors)
	}
	if len(release.Traits) != 1 || release.Traits[0] != "ship" {
		t.Errorf("release.Traits = %v, want [ship]", release.Traits)
	}
	// project.yaml's own "executor" must still be present, untouched.
	if _, ok := meta.TaskBehaviors["executor"]; !ok {
		t.Errorf("TaskBehaviors missing project.yaml's own \"executor\": %+v", meta.TaskBehaviors)
	}
}

// TestGetWithWorkspace_TaskBehaviors_ProjectYAMLWinsOnSameName pins 決定4's
// "同名は project.yaml 側が丸ごと勝つ": when both project.yaml and the
// workspace default define the same canonical name, project.yaml's
// definition survives completely untouched — no per-field merge inside the
// one behavior.
func TestGetWithWorkspace_TaskBehaviors_ProjectYAMLWinsOnSameName(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, `id: proj-win
name: Win Project
task_behaviors:
  executor:
    traits: [project-value]
`)

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "win-ws", `task_behaviors:
  executor:
    traits: [workspace-value]
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-win", WorkDir: projectDir, WorkspaceID: "win-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-win")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	executor, ok := meta.TaskBehaviors["executor"]
	if !ok {
		t.Fatalf("TaskBehaviors missing executor: %+v", meta.TaskBehaviors)
	}
	if len(executor.Traits) != 1 || executor.Traits[0] != "project-value" {
		t.Errorf("executor.Traits = %v, want [project-value] (project.yaml must win outright over the workspace default)", executor.Traits)
	}
}

// TestGetWithWorkspace_TaskBehaviors_CanonicalCollisionViaAlias pins 決定4's
// "マージは canonical 名前空間で行う" (論点j): project.yaml authoring its
// behavior under the LEGACY ALIAS name ("dev", which canonicalizes+mirrors
// to "executor" at project.yaml load time — spec_loader.go) must still be
// recognized as the SAME name as a workspace default authored under the
// CANONICAL name ("executor") — project.yaml wins, the workspace's
// "executor" must not sneak in as if it were a distinct entry.
func TestGetWithWorkspace_TaskBehaviors_CanonicalCollisionViaAlias(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, `id: proj-alias-collide
name: Alias Collide Project
task_behaviors:
  dev:
    traits: [project-value]
`)

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "alias-collide-ws", `task_behaviors:
  executor:
    traits: [workspace-value]
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-alias-collide", WorkDir: projectDir, WorkspaceID: "alias-collide-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-alias-collide")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if len(meta.TaskBehaviors) != 2 { // executor + its dev mirror — no third/distinct entry
		t.Fatalf("TaskBehaviors = %+v, want exactly 2 entries (executor + dev mirror), not a separate workspace-supplied executor", meta.TaskBehaviors)
	}
	for _, name := range []string{"executor", "dev"} {
		behavior, ok := meta.TaskBehaviors[name]
		if !ok {
			t.Fatalf("TaskBehaviors missing %q: %+v", name, meta.TaskBehaviors)
		}
		if len(behavior.Traits) != 1 || behavior.Traits[0] != "project-value" {
			t.Errorf("TaskBehaviors[%q].Traits = %v, want [project-value] (project.yaml's dev-authored entry must win, canonical-namespace-aware)", name, behavior.Traits)
		}
	}
}

// TestGetWithWorkspace_TaskBehaviors_WorkspaceSuppliedBehaviorGetsAliasMirror
// confirms a workspace-default-only canonical behavior still gets its
// legacy alias mirror added (addAliasMirrors runs on the FULL merged map,
// not just project.yaml's own entries).
func TestGetWithWorkspace_TaskBehaviors_WorkspaceSuppliedBehaviorGetsAliasMirror(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, "id: proj-mirror\nname: Mirror Project\n")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "mirror-ws", `task_behaviors:
  executor:
    traits: [impl]
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-mirror", WorkDir: projectDir, WorkspaceID: "mirror-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-mirror")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if _, ok := meta.TaskBehaviors["dev"]; !ok {
		t.Errorf("TaskBehaviors = %+v, want the \"dev\" alias mirror of the workspace-supplied \"executor\"", meta.TaskBehaviors)
	}
}

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

// TestGetWithWorkspace_BaseBranchForkPointDefaultTaskBehavior_ProjectWinsWhenSet
// is the other half of 決定5: project.yaml's own non-empty values are never
// clobbered by the workspace default.
func TestGetWithWorkspace_BaseBranchForkPointDefaultTaskBehavior_ProjectWinsWhenSet(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeProjectYAML(t, projectDir, `id: proj-override
name: Override Project
base_branch: feature/x
fork_point: origin/develop
default_task_behavior: supervisor
`)

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "override-ws", `base_branch: main
fork_point: origin/main
default_task_behavior: executor
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-override", WorkDir: projectDir, WorkspaceID: "override-ws"},
	})

	meta, err := s.GetWithWorkspace(context.Background(), "proj-override")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if meta.BaseBranch != "feature/x" {
		t.Errorf("BaseBranch = %q, want project.yaml's own feature/x", meta.BaseBranch)
	}
	if meta.ForkPoint != "origin/develop" {
		t.Errorf("ForkPoint = %q, want project.yaml's own origin/develop", meta.ForkPoint)
	}
	if meta.DefaultTaskBehavior != "supervisor" {
		t.Errorf("DefaultTaskBehavior = %q, want project.yaml's own supervisor", meta.DefaultTaskBehavior)
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
	if len(meta.TaskBehaviors) != 0 {
		t.Errorf("TaskBehaviors = %+v, want empty (no workspace default applied in the degraded window)", meta.TaskBehaviors)
	}
	if meta.BaseBranch != "" {
		t.Errorf("BaseBranch = %q, want empty", meta.BaseBranch)
	}
}
