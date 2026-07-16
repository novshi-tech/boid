package orchestrator

import (
	"path/filepath"
	"testing"
)

// TestMaterializeWorkspaceKitsForPersist_Expands pins the fix for a real
// e2e regression found while implementing PR4 (docs/plans/
// workspace-db-consolidation.md): the workspaces table has no `kits` column
// at all, so a *WorkspaceMeta carrying a kit reference would silently lose
// that reference on WorkspaceRepository.Create/Save — and, unlike the
// migration path (which runs materializeKitRuntimeIntoWorkspace before its
// own save), POST/PUT /api/workspaces had no equivalent step. A workspace
// created or auto-created (via `boid workspace assign`'s legacy-yaml
// pre-check) from a yaml that still references kits would then dispatch
// with the kit's env/host_commands/additional_bindings silently missing —
// exactly the docker-proxy-* e2e scenarios' "$DOCKER_PROXY_TEST_ROOT/
// docker-proxy-test.sh: not found" (exit 127) failure mode.
//
// Phase 2.5 PR7 (decision 12) removed WorkspaceMeta.Kits outright: kitRefs
// is now an explicit parameter the caller sources itself (the one remaining
// caller, cmd/workspace.go's ensureWorkspaceExistsForAssign, extracts it
// from the raw shadow yaml) instead of a struct field this function reads
// and clears.
func TestMaterializeWorkspaceKitsForPersist_Expands(t *testing.T) {
	kitsDir := t.TempDir()
	writeMigrationKitYAML(t, kitsDir, "toolkit", ""+
		"host_commands:\n  gh:\n    allow: [pr]\n"+
		"env:\n  KIT_VAR: from-kit\n"+
		"additional_bindings:\n  - source: /opt/kit-tool\n    target: /opt/kit-tool\n    mode: ro\n")

	meta := &WorkspaceMeta{
		Env: map[string]string{"WORKSPACE_VAR": "from-workspace"},
	}
	if err := MaterializeWorkspaceKitsForPersist(kitsDir, []string{"toolkit"}, meta); err != nil {
		t.Fatalf("MaterializeWorkspaceKitsForPersist: %v", err)
	}

	if !equalStringSlice(meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", meta.HostCommands)
	}
	if meta.Env["KIT_VAR"] != "from-kit" {
		t.Errorf("Env[KIT_VAR] = %q, want from-kit", meta.Env["KIT_VAR"])
	}
	if meta.Env["WORKSPACE_VAR"] != "from-workspace" {
		t.Errorf("Env[WORKSPACE_VAR] = %q, want from-workspace (workspace-authored env must survive)", meta.Env["WORKSPACE_VAR"])
	}
	if _, ok := findBindMountBySource(meta.AdditionalBindings, "/opt/kit-tool"); !ok {
		t.Errorf("AdditionalBindings = %+v, want an entry for /opt/kit-tool", meta.AdditionalBindings)
	}
	if _, ok := findBindMountBySource(meta.AdditionalBindings, filepath.Join(kitsDir, "toolkit")); !ok {
		t.Errorf("AdditionalBindings = %+v, want an entry for the kit root dir (KitRoots equivalent)", meta.AdditionalBindings)
	}
}

// TestMaterializeWorkspaceKitsForPersist_NoOpWhenKitRefsEmpty verifies the
// fast path never touches the filesystem for the overwhelming majority of
// workspaces (which never reference a kit) — kitsDir need not even exist.
func TestMaterializeWorkspaceKitsForPersist_NoOpWhenKitRefsEmpty(t *testing.T) {
	meta := &WorkspaceMeta{HostCommands: []string{"gh"}}
	if err := MaterializeWorkspaceKitsForPersist("/nonexistent/kits/dir", nil, meta); err != nil {
		t.Fatalf("MaterializeWorkspaceKitsForPersist: %v", err)
	}
	if !equalStringSlice(meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands mutated unexpectedly: %v", meta.HostCommands)
	}
}

// TestMaterializeWorkspaceKitsForPersist_UnresolvedKitErrors verifies that a
// kitRefs entry with no corresponding kit.yaml aborts with a clear error
// rather than silently dropping the reference (matching the migration's own
// abort-on-unresolved contract, MAJOR 2 codex review).
func TestMaterializeWorkspaceKitsForPersist_UnresolvedKitErrors(t *testing.T) {
	kitsDir := t.TempDir()
	meta := &WorkspaceMeta{}
	err := MaterializeWorkspaceKitsForPersist(kitsDir, []string{"ghost-kit"}, meta)
	if err == nil {
		t.Fatal("expected error for unresolved kit reference, got nil")
	}
}
