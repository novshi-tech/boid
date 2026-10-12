package dispatcher

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestSessionIgnoresCachedProjectBindings(t *testing.T) {
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
	if hasBindingSource(meta.AdditionalBindings, projectBind) {
		t.Fatalf("upstream: project binding %s missing from hydrated meta.AdditionalBindings: %+v", projectBind, meta.AdditionalBindings)
	}

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

	if hasBindTarget(result.Mounts, projectBind) {
		t.Errorf("project binding dropped downstream: expected bind at %s, got mounts=%+v", projectBind, result.Mounts)
	}
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
