package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestBindingPassthrough_HydrateToSandboxSpec is the end-to-end half of Tier 1
// #1 (docs/plans/quality-gates.md). It threads a workspace-kit binding through
// the full two-tier path that the 2026-06-29 regression broke:
//
//	project.yaml + workspace kit fixture
//	  → ProjectStore.GetWithWorkspace           (upstream hydrate)
//	  → meta.AdditionalBindings assert
//	  → BuildSessionJobSpec                      (the real meta→JobSpec seam)
//	  → BuildSandboxSpec                         (downstream)
//	  → sandbox mounts contain BOTH the kit bind AND the harness bind
//
// Either tier regressing — an upstream drop in GetWithWorkspace, or a
// downstream exclusive replace of kit bindings by harness bindings — makes this
// fail. The two tiers also have their own focused unit tests
// (TestGetWithWorkspace_AdditionalBindingsMerge upstream;
// TestBuildSandboxSpec_ProfileDefault_HarnessKeepsAdditionalBindings
// downstream); this test guards the seam between them, which no single-package
// test can see.
func TestBindingPassthrough_HydrateToSandboxSpec(t *testing.T) {
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Skip("hostHomeDir() returned empty; cannot exercise mount layout")
	}

	projectDir := t.TempDir()
	writeThruProjectYAML(t, projectDir, "proj-thru")

	const kitBind = "/opt/volta"
	kitsDir := t.TempDir()
	writeThruKitYAML(t, kitsDir, "volta", kitBind)

	wsDir := t.TempDir()
	writeThruWorkspaceYAML(t, wsDir, "thruws", "volta")

	// --- upstream: hydrate project meta with workspace kits ---
	store := orchestrator.NewProjectStore(orchestrator.NewRegistry(kitsDir))
	store.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	if errs := store.LoadAll([]*orchestrator.Project{
		{ID: "proj-thru", WorkDir: projectDir, WorkspaceID: "thruws"},
	}); len(errs) > 0 {
		t.Fatalf("LoadAll: %v", errs)
	}
	meta, err := store.GetWithWorkspace(context.Background(), "proj-thru")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if !hasBindingSource(meta.AdditionalBindings, kitBind) {
		t.Fatalf("upstream: kit binding %s missing from hydrated meta.AdditionalBindings: %+v", kitBind, meta.AdditionalBindings)
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

	if !hasBindTarget(result.Mounts, kitBind) {
		t.Errorf("kit binding dropped downstream: expected bind at %s, got mounts=%+v", kitBind, result.Mounts)
	}
	// The harness bindings must survive alongside the kit binding (added on top,
	// not replaced). claude.Adapter.Bindings declares a ~/.claude rw bind.
	claudeDir := filepath.Join(homeDir, ".claude")
	if !hasBindTarget(result.Mounts, claudeDir) {
		t.Errorf("harness binding dropped: expected claude bind at %s, got mounts=%+v", claudeDir, result.Mounts)
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

func writeThruProjectYAML(t *testing.T, dir, id string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	y := "id: " + id + "\nname: " + id + "\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(y), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
}

func writeThruKitYAML(t *testing.T, baseDir, slug, bindSource string) {
	t.Helper()
	kitDir := filepath.Join(baseDir, slug)
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit dir: %v", err)
	}
	y := "additional_bindings:\n  - source: " + bindSource + "\n    target: " + bindSource + "\n    mode: rw\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(y), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

func writeThruWorkspaceYAML(t *testing.T, dir, slug, kitRef string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir ws dir: %v", err)
	}
	y := "kits:\n  - " + kitRef + "\n"
	if err := os.WriteFile(filepath.Join(dir, slug+".yaml"), []byte(y), 0o644); err != nil {
		t.Fatalf("write workspace yaml: %v", err)
	}
}
