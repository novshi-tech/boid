package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// writeTestWorkspaceYAML writes a workspace yaml fixture under
// <xdgConfigHome>/boid/workspaces/<slug>.yaml — the exact path
// buildProjectStore's MigrateWorkspaceYAMLToDB call resolves via
// orchestrator.DefaultWorkspaceDir() when workspaceDir is passed as "".
func writeTestWorkspaceYAML(t *testing.T, xdgConfigHome, slug, content string) {
	t.Helper()
	dir := filepath.Join(xdgConfigHome, "boid", "workspaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write workspace yaml %q: %v", slug, err)
	}
}

// setupProjectYAML writes a minimal .boid/project.yaml with one task
// behavior so per-behavior hydration is exercised (mirrors
// project_store_hydrate_test.go's setupProjectWithBehavior).
func setupProjectYAML(t *testing.T, dir, id, behavior string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	yaml := "id: " + id + "\nname: " + id + "\ntask_behaviors:\n  " + behavior + ":\n    name: " + behavior + "\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
}

// TestBuildProjectStore_MigratesWorkspaceYAMLAndDispatchWorks is the Step H
// parity check (docs/plans/workspace-db-consolidation.md PR3): a fresh
// daemon startup (no schema_migrations row for workspace_db_consolidation
// yet) must migrate the on-disk workspace yaml (with a kit reference) into
// the workspaces table, and GetWithWorkspace for a project assigned to that
// workspace must resolve exactly as it did pre-cutover (kit host_commands +
// env), while also picking up the migrated ws.HostCommands via the
// aggregated config now wired through SetHostCommands.
func TestBuildProjectStore_MigratesWorkspaceYAMLAndDispatchWorks(t *testing.T) {
	xdgConfigHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr, issue]\n")
	writeTestWorkspaceYAML(t, xdgConfigHome, "team-a", "kits:\n  - gh-kit\nenv:\n  FOO: bar\n")

	projectDir := t.TempDir()
	setupProjectYAML(t, projectDir, "proj-parity", "build")

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-parity", WorkDir: projectDir}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-parity", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	cfg := Config{DBPath: ":memory:", KitsDir: kitsDir}
	store, _, err := buildProjectStore(cfg, d.Conn, repo)
	if err != nil {
		t.Fatalf("buildProjectStore: %v", err)
	}

	meta, err := store.GetWithWorkspace(context.Background(), "proj-parity")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if meta.Env["FOO"] != "bar" {
		t.Errorf("expected workspace env FOO=bar, got %v", meta.Env)
	}
	gh, ok := meta.HostCommands["gh"]
	if !ok {
		t.Fatalf("expected 'gh' host_command hydrated from workspace kit, got %+v", meta.HostCommands)
	}
	if len(gh.Allow) != 2 {
		t.Errorf("gh.Allow = %v, want 2 entries", gh.Allow)
	}

	// The workspace must now be DB-backed: WorkspaceStore().List() reflects
	// the migrated data, including the auto-created default workspace.
	wsStore := store.WorkspaceStore()
	if wsStore == nil {
		t.Fatal("expected WorkspaceStore to be wired")
	}
	slugs, err := wsStore.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := map[string]bool{}
	for _, s := range slugs {
		found[s] = true
	}
	if !found["team-a"] || !found[orchestrator.DefaultWorkspaceSlug] {
		t.Errorf("expected workspaces [team-a, default] to be migrated, got %v", slugs)
	}
}

// TestBuildProjectStore_SecondStartupSkipsMigrationAndDispatchStillWorks
// covers the "2 回目起動 -> migration skip、dispatch 動く" Step H case: after
// a first buildProjectStore call has committed the migration, a second call
// against the SAME db connection (simulating a daemon restart) must not
// re-run the migration (no error even though the yaml is now stale/ignored)
// and GetWithWorkspace must still resolve the workspace exactly as before.
func TestBuildProjectStore_SecondStartupSkipsMigrationAndDispatchStillWorks(t *testing.T) {
	xdgConfigHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	writeTestWorkspaceYAML(t, xdgConfigHome, "team-b", "env:\n  FIRST: run\n")

	projectDir := t.TempDir()
	setupProjectYAML(t, projectDir, "proj-restart", "build")

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-restart", WorkDir: projectDir}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-restart", "team-b"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	cfg := Config{DBPath: ":memory:"}
	if _, _, err := buildProjectStore(cfg, d.Conn, repo); err != nil {
		t.Fatalf("first buildProjectStore: %v", err)
	}

	// Mutate the on-disk yaml after the first (committed) migration: since
	// the migration is a one-time cutover, this change must NOT reach the
	// DB on the second call.
	writeTestWorkspaceYAML(t, xdgConfigHome, "team-b", "env:\n  FIRST: should-not-be-seen\n  SECOND: also-not-seen\n")

	store, _, err := buildProjectStore(cfg, d.Conn, repo)
	if err != nil {
		t.Fatalf("second buildProjectStore (restart): %v", err)
	}

	meta, err := store.GetWithWorkspace(context.Background(), "proj-restart")
	if err != nil {
		t.Fatalf("GetWithWorkspace after restart: %v", err)
	}
	if meta.Env["FIRST"] != "run" {
		t.Errorf("expected DB-authoritative env FIRST=run (migrated once), got %v", meta.Env)
	}
	if _, ok := meta.Env["SECOND"]; ok {
		t.Errorf("post-cutover yaml edits must not reach the DB: meta.Env=%v", meta.Env)
	}
}

// hasBindingSource reports whether mounts contains an entry whose Source
// equals source.
func hasBindingSource(mounts []orchestrator.BindMount, source string) bool {
	for _, m := range mounts {
		if m.Source == source {
			return true
		}
	}
	return false
}

// TestBuildProjectStore_MigratedKitRootBindingReachesDispatch pins the
// end-to-end path for the 2nd-pass codex-review BLOCKER (KitRoots
// materialization, see workspace_migration_test.go's
// TestMigrateWorkspaceYAMLToDB_MaterializesKitRootsAsAdditionalBindings for
// the unit-level coverage of the migration step itself): after
// MigrateWorkspaceYAMLToDB folds a workspace kit's root directory into the
// DB-bound WorkspaceMeta's AdditionalBindings, GetWithWorkspace's existing
// ws.AdditionalBindings merge block (BLOCKER 2 — already covered for the
// directly-authored case by project_store_hydrate_test.go's
// TestGetWithWorkspace_MergesWorkspaceAdditionalBindings) must carry that
// materialized binding all the way to dispatch: both the top-level
// meta.AdditionalBindings (session jobs bypass behaviors and read this
// directly) and every TaskBehavior.AdditionalBindings (task hooks read it
// via the planner) — the same guarantee the pre-cutover ws.Kits/KitRoots
// path provided for shell hooks reading kit-dir scripts/assets.
func TestBuildProjectStore_MigratedKitRootBindingReachesDispatch(t *testing.T) {
	xdgConfigHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	kitsDir := t.TempDir()
	writeTestKitYAML(t, kitsDir, "shellkit", "host_commands:\n  mytool:\n    path: /usr/bin/true\n")
	writeTestWorkspaceYAML(t, xdgConfigHome, "team-root", "kits:\n  - shellkit\n")

	projectDir := t.TempDir()
	setupProjectYAML(t, projectDir, "proj-kitroot", "build")

	d := openTestDB(t)
	repo := orchestrator.NewProjectRepository(d.Conn)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-kitroot", WorkDir: projectDir}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-kitroot", "team-root"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	cfg := Config{DBPath: ":memory:", KitsDir: kitsDir}
	store, _, err := buildProjectStore(cfg, d.Conn, repo)
	if err != nil {
		t.Fatalf("buildProjectStore: %v", err)
	}

	meta, err := store.GetWithWorkspace(context.Background(), "proj-kitroot")
	if err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}

	wantKitDir := filepath.Join(kitsDir, "shellkit")
	if !hasBindingSource(meta.AdditionalBindings, wantKitDir) {
		t.Fatalf("expected kit root %q in meta.AdditionalBindings, got %+v", wantKitDir, meta.AdditionalBindings)
	}

	build, ok := meta.TaskBehaviors["build"]
	if !ok {
		t.Fatalf("behavior build missing from hydrated meta: %+v", meta.TaskBehaviors)
	}
	if !hasBindingSource(build.AdditionalBindings, wantKitDir) {
		t.Fatalf("expected kit root %q in behavior build.AdditionalBindings, got %+v", wantKitDir, build.AdditionalBindings)
	}
}
