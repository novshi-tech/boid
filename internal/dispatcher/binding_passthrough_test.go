package dispatcher

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestBindingPassthrough_HydrateToSandboxSpec is the end-to-end half of Tier 1
// #1 (docs/plans/quality-gates.md). It threads a project-level binding
// through the full two-tier path that the 2026-06-29 regression broke:
//
//	ProjectMeta.AdditionalBindings
//	  → ProjectStore.GetWithWorkspace           (upstream hydrate, passthrough)
//	  → meta.AdditionalBindings assert
//	  → BuildSessionJobSpec                      (the real meta→JobSpec seam)
//	  → BuildSandboxSpec                         (downstream)
//	  → sandbox mounts contain BOTH the project bind AND the harness bind
//
// A downstream exclusive replace of project bindings by harness bindings
// makes this fail. The downstream half also has its own focused unit test
// (TestBuildSandboxSpec_ProfileDefault_HarnessKeepsAdditionalBindings); this
// test guards the seam from meta hydration all the way to sandbox mounts,
// which no single-package test can see.
//
// This test originally threaded the binding through a workspace *kit*
// fixture (ws.Kits → kit.yaml's additional_bindings; retired in docs/plans/
// workspace-db-consolidation.md Phase 2.5 PR6), then through workspace.yaml's
// own additional_bindings field directly (decision 4). docs/plans/
// home-workspace-volume.md Phase 4 PR4 retired the workspace-level
// AdditionalBindings mechanism outright — WorkspaceMeta has no field for it
// any more, and GetWithWorkspace no longer merges anything from it — so the
// vehicle is now a directly-constructed ProjectMeta.AdditionalBindings
// (project.yaml's own on-disk parsing still rejects a top-level
// additional_bindings key post-cutover; see spec_loader.go's
// removedTopLevelKeys), the same way project_store_hydrate_test.go's
// TestGetWithWorkspace_ProjectBindingsWinOnConflict isolates this from that
// unrelated schema-migration concern. This still drives the identical
// downstream half of the seam (BuildSessionJobSpec → BuildSandboxSpec) the
// original regression broke.
func TestBindingPassthrough_HydrateToSandboxSpec(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}

	projectDir := t.TempDir()

	const projectBind = "/opt/volta"

	// --- upstream: hydrate project meta with a project-level binding ---
	store := orchestrator.NewProjectStore()
	store.Set("proj-thru", &orchestrator.ProjectMeta{
		ID:   "proj-thru",
		Name: "proj-thru",
		AdditionalBindings: []orchestrator.BindMount{
			{Source: projectBind, Target: projectBind, Mode: "rw"},
		},
	})
	// GetWithWorkspace (not the bare Get) is the same hydration entry point
	// production dispatch calls — with no linked workspace, it degrades to
	// returning the cached meta unchanged, but exercising it here (rather
	// than Get) keeps this test on the real call path.
	meta, err := store.GetWithWorkspace(context.Background(), "proj-thru")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if !hasBindingSource(meta.AdditionalBindings, projectBind) {
		t.Fatalf("upstream: project binding %s missing from hydrated meta.AdditionalBindings: %+v", projectBind, meta.AdditionalBindings)
	}

	// --- the seam: drive the real session dispatch conversion. BuildSessionJobSpec
	// is the production meta→JobSpec function that server/wire.go feeds
	// meta.AdditionalBindings into for POST /sessions and `boid agent`. Using it
	// here (rather than hand-building the JobSpec) means a regression that drops
	// AdditionalBindings from that conversion is also caught. ---
	//
	// projectDir is a bare t.TempDir() (not a real git repo), so stub the
	// session base-branch resolver — the fail-loud contract added by the
	// PR6 Opus review otherwise turns this into a hard error, unrelated to
	// the binding-passthrough seam this test is about.
	stubSessionBaseBranch(t, "main")
	spec, err := BuildSessionJobSpec(SessionJobInput{
		ProjectID:          "proj-thru",
		ProjectWorkDir:     projectDir,
		HarnessType:        "claude",
		AdditionalBindings: meta.AdditionalBindings,
	})
	if err != nil {
		t.Fatalf("BuildSessionJobSpec: %v", err)
	}

	// --- downstream: build the sandbox spec ---
	result, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
	if err != nil {
		t.Fatalf("BuildSandboxSpec: %v", err)
	}

	if !hasBindTarget(result.Mounts, projectBind) {
		t.Errorf("project binding dropped downstream: expected bind at %s, got mounts=%+v", projectBind, result.Mounts)
	}
	// Phase 4 PR3 (docs/plans/home-workspace-volume.md) retired
	// claude.Adapter.Bindings (it now returns nil), so there is no longer a
	// harness-declared ~/.claude bind to assert alongside the project
	// binding here — this test's remaining job is the downstream half of the
	// seam (project AdditionalBindings survive BuildSandboxSpec) pinned
	// above.
}

func hasBindingSource(bindings []orchestrator.BindMount, src string) bool {
	for _, b := range bindings {
		if b.Source == src {
			return true
		}
	}
	return false
}

func hasBindTarget(mounts []sandbox.Mount, target string) bool {
	for _, m := range mounts {
		if m.Target == target && m.Type == sandbox.MountBind {
			return true
		}
	}
	return false
}
