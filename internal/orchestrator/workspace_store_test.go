package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

// newTestDBBackedStore returns a WorkspaceStore wired to a fresh in-memory
// SQLite DB via NewWorkspaceStoreWithRepo, exercising the PR3 DB-mode branch
// of every WorkspaceStore method (docs/plans/workspace-db-consolidation.md).
func newTestDBBackedStore(t *testing.T) *WorkspaceStore {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return NewWorkspaceStoreWithRepo(t.TempDir(), NewWorkspaceRepository(d.Conn))
}

func newTestStore(t *testing.T) (*WorkspaceStore, string) {
	t.Helper()
	dir := t.TempDir()
	return NewWorkspaceStore(dir), dir
}

func TestWorkspaceStore_SaveLoad_RoundTrip(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	meta := &WorkspaceMeta{
		HostCommands: []string{"gh"},
		Env:          map[string]string{"FOO": "bar"},
	}
	if err := store.Save("my-ws", meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load("my-ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.HostCommands) != 1 || got.HostCommands[0] != "gh" {
		t.Errorf("HostCommands: got %v, want [gh]", got.HostCommands)
	}
	if got.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO]: got %q, want %q", got.Env["FOO"], "bar")
	}
}

func TestWorkspaceStore_List_MultiSlug(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	for _, slug := range []string{"alpha", "beta", "gamma"} {
		if err := store.Save(slug, &WorkspaceMeta{}); err != nil {
			t.Fatalf("Save %q: %v", slug, err)
		}
	}

	slugs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(slugs)
	want := []string{"alpha", "beta", "gamma"}
	if len(slugs) != len(want) {
		t.Fatalf("List: got %v, want %v", slugs, want)
	}
	for i, s := range want {
		if slugs[i] != s {
			t.Errorf("List[%d]: got %q, want %q", i, slugs[i], s)
		}
	}
}

func TestWorkspaceStore_Remove_ThenLoadReturnsNotExist(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	if err := store.Save("gone", &WorkspaceMeta{HostCommands: []string{"a"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Remove("gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, err := store.Load("gone")
	if err == nil {
		t.Fatal("Load after Remove: expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load after Remove: error = %v, want os.ErrNotExist wrapped", err)
	}
}

func TestWorkspaceStore_InvalidSlug_Errors(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	invalid := []string{"", "Bad", "with space", "foo/bar", "my_kit"}
	for _, slug := range invalid {
		slug := slug
		t.Run("save/"+slug, func(t *testing.T) {
			t.Parallel()
			if err := store.Save(slug, &WorkspaceMeta{}); err == nil {
				t.Errorf("Save(%q): expected error, got nil", slug)
			}
		})
		t.Run("load/"+slug, func(t *testing.T) {
			t.Parallel()
			if _, err := store.Load(slug); err == nil {
				t.Errorf("Load(%q): expected error, got nil", slug)
			}
		})
		t.Run("remove/"+slug, func(t *testing.T) {
			t.Parallel()
			if err := store.Remove(slug); err == nil {
				t.Errorf("Remove(%q): expected error, got nil", slug)
			}
		})
	}
}

func TestWorkspaceStore_SaveOverwrite(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	if err := store.Save("ws", &WorkspaceMeta{HostCommands: []string{"old"}}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Save("ws", &WorkspaceMeta{HostCommands: []string{"new"}}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := store.Load("ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.HostCommands) != 1 || got.HostCommands[0] != "new" {
		t.Errorf("HostCommands after overwrite: got %v, want [new]", got.HostCommands)
	}
}

func TestWorkspaceStore_ListDirNotExist_EmptyNilErr(t *testing.T) {
	t.Parallel()
	// Use a sub-directory that does not exist.
	store := NewWorkspaceStore(filepath.Join(t.TempDir(), "no-such-dir"))

	slugs, err := store.List()
	if err != nil {
		t.Fatalf("List on missing dir: expected nil error, got %v", err)
	}
	if len(slugs) != 0 {
		t.Errorf("List on missing dir: expected empty, got %v", slugs)
	}
}

func TestWorkspaceStore_LoadParseError(t *testing.T) {
	t.Parallel()
	store, dir := newTestStore(t)

	// Write deliberately broken YAML.
	badYAML := []byte("kits: [unclosed bracket\n")
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), badYAML, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := store.Load("broken")
	if err == nil {
		t.Fatal("Load with bad YAML: expected error, got nil")
	}
}

func TestWorkspaceStore_RemoveNotExist(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	err := store.Remove("nonexistent")
	if err == nil {
		t.Fatal("Remove non-existent: expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove non-existent: error = %v, want os.ErrNotExist wrapped", err)
	}
}

func TestWorkspaceStore_EnsureDefault_CreatesEmptyWhenMissing(t *testing.T) {
	t.Parallel()
	store, dir := newTestStore(t)

	if err := store.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}

	// File must exist after EnsureDefault.
	path := filepath.Join(dir, DefaultWorkspaceSlug+".yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default.yaml not created: %v", err)
	}

	// Load must succeed and return an empty meta (no HostCommands / Env / Capabilities).
	meta, err := store.Load(DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("Load(default): %v", err)
	}
	if len(meta.HostCommands) != 0 {
		t.Errorf("HostCommands: got %v, want empty", meta.HostCommands)
	}
	if len(meta.Env) != 0 {
		t.Errorf("Env: got %v, want empty", meta.Env)
	}
	if meta.Capabilities.Docker != nil {
		t.Errorf("Capabilities.Docker: got %v, want nil", meta.Capabilities.Docker)
	}
}

func TestWorkspaceStore_EnsureDefault_PreservesExisting(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	original := &WorkspaceMeta{
		HostCommands: []string{"user-edited"},
		Env:          map[string]string{"USER_SET": "yes"},
	}
	if err := store.Save(DefaultWorkspaceSlug, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := store.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}

	meta, err := store.Load(DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("Load(default): %v", err)
	}
	if len(meta.HostCommands) != 1 || meta.HostCommands[0] != "user-edited" {
		t.Errorf("user edits clobbered: HostCommands = %v", meta.HostCommands)
	}
	if meta.Env["USER_SET"] != "yes" {
		t.Errorf("user edits clobbered: Env[USER_SET] = %q", meta.Env["USER_SET"])
	}
}

func TestWorkspaceStore_Remove_RejectsDefault(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	if err := store.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}

	err := store.Remove(DefaultWorkspaceSlug)
	if err == nil {
		t.Fatal("Remove(default): expected error, got nil")
	}

	// The file must still exist.
	if _, loadErr := store.Load(DefaultWorkspaceSlug); loadErr != nil {
		t.Errorf("default.yaml was deleted despite rejected Remove: %v", loadErr)
	}
}

func TestWorkspaceStore_SaveAtomic_TmpFileInSameDir(t *testing.T) {
	t.Parallel()
	// This test verifies that the atomic write uses a tmp file in the same
	// directory (ensuring same-filesystem rename). We do this by checking that
	// after a successful Save, only the final .yaml file exists (no leftover
	// .tmp files) and that its content is valid.
	store, dir := newTestStore(t)

	meta := &WorkspaceMeta{HostCommands: []string{"check-atomic"}}
	if err := store.Save("atomic-ws", meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" {
			t.Errorf("unexpected file after Save: %q (expected only .yaml files)", e.Name())
		}
	}
}

// --- DB-mode (repo != nil) tests: docs/plans/workspace-db-consolidation.md PR3 ---

func TestWorkspaceStore_DBMode_SaveLoad_RoundTrip(t *testing.T) {
	t.Parallel()
	store := newTestDBBackedStore(t)

	meta := &WorkspaceMeta{
		HostCommands: []string{"gh"},
		Env:          map[string]string{"FOO": "bar"},
	}
	if err := store.Save("my-ws", meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load("my-ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalStringSlice(got.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands: got %v, want [gh]", got.HostCommands)
	}
	if got.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO]: got %q, want %q", got.Env["FOO"], "bar")
	}
}

func TestWorkspaceStore_DBMode_List_MultiSlug(t *testing.T) {
	t.Parallel()
	store := newTestDBBackedStore(t)

	for _, slug := range []string{"alpha", "beta", "gamma"} {
		if err := store.Save(slug, &WorkspaceMeta{}); err != nil {
			t.Fatalf("Save %q: %v", slug, err)
		}
	}

	slugs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(slugs)
	want := []string{"alpha", "beta", "gamma"}
	if !equalStringSlice(slugs, want) {
		t.Errorf("List: got %v, want %v", slugs, want)
	}
}

func TestWorkspaceStore_DBMode_Remove_ThenLoadReturnsNotExist(t *testing.T) {
	t.Parallel()
	store := newTestDBBackedStore(t)

	if err := store.Save("gone", &WorkspaceMeta{HostCommands: []string{"a"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Remove("gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, err := store.Load("gone")
	if err == nil {
		t.Fatal("Load after Remove: expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load after Remove: error = %v, want os.ErrNotExist wrapped", err)
	}
}

// TestWorkspaceStore_DBMode_LoadWithRevision_DelegatesToRepo pins that
// WorkspaceStore's LoadWithRevision/UpdateIfRevisionMatches wrappers
// (docs/plans/workspace-db-consolidation.md MAJOR 1) route through the
// repository in DB mode, exactly like every other method on this struct.
func TestWorkspaceStore_DBMode_LoadWithRevision_DelegatesToRepo(t *testing.T) {
	t.Parallel()
	store := newTestDBBackedStore(t)

	if err := store.Save("team-a", &WorkspaceMeta{HostCommands: []string{"gh"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	meta, revision, err := store.LoadWithRevision("team-a")
	if err != nil {
		t.Fatalf("LoadWithRevision: %v", err)
	}
	if !equalStringSlice(meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", meta.HostCommands)
	}
	if revision == "" {
		t.Error("expected non-empty revision")
	}

	newRevision, matched, err := store.UpdateIfRevisionMatches("team-a", revision, &WorkspaceMeta{HostCommands: []string{"aws"}})
	if err != nil {
		t.Fatalf("UpdateIfRevisionMatches: %v", err)
	}
	if !matched {
		t.Fatal("expected matched=true for the correct revision")
	}
	if newRevision == revision {
		t.Errorf("newRevision = %q, want distinct from original %q", newRevision, revision)
	}

	got, err := store.Load("team-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalStringSlice(got.HostCommands, []string{"aws"}) {
		t.Errorf("HostCommands after update = %v, want [aws]", got.HostCommands)
	}

	// A stale revision must be rejected.
	_, matched, err = store.UpdateIfRevisionMatches("team-a", revision, &WorkspaceMeta{HostCommands: []string{"stale-writer"}})
	if err != nil {
		t.Fatalf("UpdateIfRevisionMatches (stale): %v", err)
	}
	if matched {
		t.Error("expected matched=false for a stale revision")
	}
}

func TestWorkspaceStore_DBMode_Remove_RejectsDefault(t *testing.T) {
	t.Parallel()
	store := newTestDBBackedStore(t)

	if err := store.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	if err := store.Remove(DefaultWorkspaceSlug); err == nil {
		t.Fatal("Remove(default): expected error, got nil")
	}
	if _, err := store.Load(DefaultWorkspaceSlug); err != nil {
		t.Errorf("default workspace was deleted despite rejected Remove: %v", err)
	}
}

func TestWorkspaceStore_DBMode_EnsureDefault_Idempotent(t *testing.T) {
	t.Parallel()
	store := newTestDBBackedStore(t)

	if err := store.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault (1st): %v", err)
	}
	if err := store.Save(DefaultWorkspaceSlug, &WorkspaceMeta{Env: map[string]string{"USER_SET": "yes"}}); err != nil {
		t.Fatalf("Save(default): %v", err)
	}
	if err := store.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault (2nd): %v", err)
	}

	meta, err := store.Load(DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("Load(default): %v", err)
	}
	if meta.Env["USER_SET"] != "yes" {
		t.Errorf("EnsureDefault clobbered existing default workspace: Env=%v", meta.Env)
	}
}

// TestWorkspaceStore_DBMode_Create_InsertsAndConflicts pins WorkspaceStore's
// Create pass-through (docs/plans/workspace-db-consolidation.md Step A):
// in DB mode it must delegate straight to the repository's insert-only
// Create, including the os.ErrExist conflict contract.
func TestWorkspaceStore_DBMode_Create_InsertsAndConflicts(t *testing.T) {
	t.Parallel()
	store := newTestDBBackedStore(t)

	meta := &WorkspaceMeta{HostCommands: []string{"gh"}}
	if err := store.Create("new-ws", meta); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Load("new-ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalStringSlice(got.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands: got %v, want [gh]", got.HostCommands)
	}

	err = store.Create("new-ws", &WorkspaceMeta{})
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("Create (conflict): error = %v, want os.ErrExist wrapped", err)
	}
}

// TestWorkspaceStore_YAMLMode_Create_InsertsAndConflicts verifies the
// yaml-mode (repo == nil) counterpart: Create still enforces insert-only
// semantics against the yaml directory, even though production wiring
// always sets a repository (this mode only matters for tests / any
// caller that constructs a bare WorkspaceStore).
func TestWorkspaceStore_YAMLMode_Create_InsertsAndConflicts(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	if err := store.Create("new-ws", &WorkspaceMeta{HostCommands: []string{"a"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Load("new-ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.HostCommands) != 1 || got.HostCommands[0] != "a" {
		t.Errorf("HostCommands: got %v, want [a]", got.HostCommands)
	}

	err = store.Create("new-ws", &WorkspaceMeta{})
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("Create (conflict): error = %v, want os.ErrExist wrapped", err)
	}
}

// TestWorkspaceStore_YAMLMode_LoadWithRevision_EmptyRevision pins the
// yaml-mode (repo == nil) fallback for LoadWithRevision/
// UpdateIfRevisionMatches (docs/plans/workspace-db-consolidation.md MAJOR
// 1): yaml mode has no revision concept (production wiring always uses DB
// mode for the workspace CRUD endpoints that care about revisions), so
// LoadWithRevision reports an empty revision string, and
// UpdateIfRevisionMatches always reports matched=true (an unconditional
// Save, matching this mode's pre-existing non-CAS Save semantics) rather
// than erroring or synthesizing a fake revision concept.
func TestWorkspaceStore_YAMLMode_LoadWithRevision_EmptyRevision(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	if err := store.Save("team-a", &WorkspaceMeta{HostCommands: []string{"gh"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	meta, revision, err := store.LoadWithRevision("team-a")
	if err != nil {
		t.Fatalf("LoadWithRevision: %v", err)
	}
	if !equalStringSlice(meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", meta.HostCommands)
	}
	if revision != "" {
		t.Errorf("revision = %q, want empty in yaml mode", revision)
	}

	_, matched, err := store.UpdateIfRevisionMatches("team-a", "anything", &WorkspaceMeta{HostCommands: []string{"aws"}})
	if err != nil {
		t.Fatalf("UpdateIfRevisionMatches: %v", err)
	}
	if !matched {
		t.Error("expected matched=true in yaml mode (unconditional Save)")
	}
	got, err := store.Load("team-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalStringSlice(got.HostCommands, []string{"aws"}) {
		t.Errorf("HostCommands after update = %v, want [aws]", got.HostCommands)
	}
}

// TestWorkspaceStore_DBMode_SetRepository_SwitchesFromYAMLMode pins PR4's
// removal of PR3's transitional yaml fallback (docs/plans/
// workspace-db-consolidation.md Step J): once repo is wired, Load must
// route through the DB exclusively and never fall back to reading
// <dir>/<slug>.yaml, even for a slug that only ever existed as a yaml file
// (the DB-mode window's degraded-fallback case PR3 introduced). PR4 gives
// every caller a real way to introduce a DB row (POST /api/workspaces / the
// CLI's assign auto-create) instead, so a yaml-only slug surfacing
// os.ErrNotExist here is the intended contract, not a regression.
func TestWorkspaceStore_DBMode_SetRepository_SwitchesFromYAMLMode(t *testing.T) {
	t.Parallel()

	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// Start in yaml mode (matches NewWorkspaceStore("") in existing callers),
	// write a workspace to the yaml dir, then switch to DB mode via
	// SetRepository.
	dir := t.TempDir()
	store := NewWorkspaceStore(dir)
	if err := store.Save("yaml-only", &WorkspaceMeta{HostCommands: []string{"yaml"}}); err != nil {
		t.Fatalf("Save (yaml mode): %v", err)
	}

	store.SetRepository(NewWorkspaceRepository(d.Conn))

	// yaml-only was never migrated into the DB, and DB mode no longer falls
	// back to the yaml file — must surface os.ErrNotExist.
	if _, err := store.Load("yaml-only"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load(yaml-only) after SetRepository: error = %v, want os.ErrNotExist (fallback removed)", err)
	}

	if err := store.Save("db-only", &WorkspaceMeta{HostCommands: []string{"db"}}); err != nil {
		t.Fatalf("Save (db mode): %v", err)
	}
	got, err := store.Load("db-only")
	if err != nil {
		t.Fatalf("Load(db-only): %v", err)
	}
	if !equalStringSlice(got.HostCommands, []string{"db"}) {
		t.Errorf("HostCommands: got %v, want [db]", got.HostCommands)
	}

	// The yaml file must still exist on disk untouched (shadow copy for
	// rollback/export, decision 16 — PR4 does not delete it either).
	if _, err := os.Stat(filepath.Join(dir, "yaml-only.yaml")); err != nil {
		t.Errorf("expected yaml-only.yaml to still exist on disk: %v", err)
	}

	// A slug that exists in neither the DB nor as a yaml file must still
	// surface os.ErrNotExist, so callers that branch on that error
	// (dispatcher's degraded-mode path) still get the right signal.
	if _, err := store.Load("missing-everywhere"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load(missing-everywhere): expected os.ErrNotExist, got %v", err)
	}
}
