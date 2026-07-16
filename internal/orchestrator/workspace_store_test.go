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
		Kits: []string{"go-tools"},
		Env:  map[string]string{"FOO": "bar"},
	}
	if err := store.Save("my-ws", meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load("my-ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Kits) != 1 || got.Kits[0] != "go-tools" {
		t.Errorf("Kits: got %v, want [go-tools]", got.Kits)
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

	if err := store.Save("gone", &WorkspaceMeta{Kits: []string{"a"}}); err != nil {
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

	if err := store.Save("ws", &WorkspaceMeta{Kits: []string{"old"}}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Save("ws", &WorkspaceMeta{Kits: []string{"new"}}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := store.Load("ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Kits) != 1 || got.Kits[0] != "new" {
		t.Errorf("Kits after overwrite: got %v, want [new]", got.Kits)
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

	// Load must succeed and return an empty meta (no Kits / Env / Capabilities).
	meta, err := store.Load(DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("Load(default): %v", err)
	}
	if len(meta.Kits) != 0 {
		t.Errorf("Kits: got %v, want empty", meta.Kits)
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
		Kits: []string{"user-edited"},
		Env:  map[string]string{"USER_SET": "yes"},
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
	if len(meta.Kits) != 1 || meta.Kits[0] != "user-edited" {
		t.Errorf("user edits clobbered: Kits = %v", meta.Kits)
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

	meta := &WorkspaceMeta{Kits: []string{"check-atomic"}}
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
	// SetRepository and verify the yaml-written workspace is NOT visible
	// through the DB-backed Load (the two are deliberately independent
	// stores once the repository is wired — the daemon startup path relies
	// on MigrateWorkspaceYAMLToDB, not on WorkspaceStore itself, to bridge
	// yaml content into the DB).
	dir := t.TempDir()
	store := NewWorkspaceStore(dir)
	if err := store.Save("yaml-only", &WorkspaceMeta{HostCommands: []string{"yaml"}}); err != nil {
		t.Fatalf("Save (yaml mode): %v", err)
	}

	store.SetRepository(NewWorkspaceRepository(d.Conn))

	if _, err := store.Load("yaml-only"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load(yaml-only) after SetRepository: expected os.ErrNotExist, got %v", err)
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

	// The yaml file must still exist on disk untouched (shadow copy, PR3
	// does not delete it).
	if _, err := os.Stat(filepath.Join(dir, "yaml-only.yaml")); err != nil {
		t.Errorf("expected yaml-only.yaml to still exist on disk: %v", err)
	}
}
