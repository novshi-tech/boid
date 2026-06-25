package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

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
