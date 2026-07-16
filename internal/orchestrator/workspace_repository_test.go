package orchestrator

import (
	"errors"
	"os"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

// newTestWorkspaceRepo returns a WorkspaceRepository backed by a fresh
// in-memory SQLite DB with every migration applied (workspaces +
// project_workspaces tables included).
func newTestWorkspaceRepo(t *testing.T) *WorkspaceRepository {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return NewWorkspaceRepository(d.Conn)
}

func TestWorkspaceRepository_Load_NotExist(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	_, err := repo.Load("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent workspace, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist wrapped, got: %v", err)
	}
}

func TestWorkspaceRepository_SaveLoad_RoundTrip(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	meta := &WorkspaceMeta{
		HostCommands:   []string{"gh", "aws"},
		Env:            map[string]string{"FOO": "bar"},
		AllowedDomains: []string{"example.com"},
		ExtraRepos:     []string{"https://github.com/example/lib.git"},
		Capabilities:   Capabilities{Docker: &DockerCapability{}},
		AdditionalBindings: []BindMount{
			{Source: "/opt/volta", Target: "/opt/volta", Mode: "rw"},
		},
		ContainerImage: "ghcr.io/example/image:latest",
	}
	if err := repo.Save("my-ws", meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.Load("my-ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalStringSlice(got.HostCommands, meta.HostCommands) {
		t.Errorf("HostCommands: got %v, want %v", got.HostCommands, meta.HostCommands)
	}
	if got.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO]: got %q, want %q", got.Env["FOO"], "bar")
	}
	if !equalStringSlice(got.AllowedDomains, meta.AllowedDomains) {
		t.Errorf("AllowedDomains: got %v, want %v", got.AllowedDomains, meta.AllowedDomains)
	}
	if !equalStringSlice(got.ExtraRepos, meta.ExtraRepos) {
		t.Errorf("ExtraRepos: got %v, want %v", got.ExtraRepos, meta.ExtraRepos)
	}
	if got.Capabilities.Docker == nil {
		t.Error("Capabilities.Docker: got nil, want non-nil")
	}
	if len(got.AdditionalBindings) != 1 || got.AdditionalBindings[0] != meta.AdditionalBindings[0] {
		t.Errorf("AdditionalBindings: got %+v, want %+v", got.AdditionalBindings, meta.AdditionalBindings)
	}
	if got.ContainerImage != meta.ContainerImage {
		t.Errorf("ContainerImage: got %q, want %q", got.ContainerImage, meta.ContainerImage)
	}
}

func TestWorkspaceRepository_Save_EmptyMetaRoundTripsToEmptyJSONColumns(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Save("empty-ws", &WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.Load("empty-ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.HostCommands) != 0 {
		t.Errorf("HostCommands: got %v, want empty", got.HostCommands)
	}
	if len(got.Env) != 0 {
		t.Errorf("Env: got %v, want empty", got.Env)
	}
	if len(got.AllowedDomains) != 0 {
		t.Errorf("AllowedDomains: got %v, want empty", got.AllowedDomains)
	}
	if len(got.ExtraRepos) != 0 {
		t.Errorf("ExtraRepos: got %v, want empty", got.ExtraRepos)
	}
	if got.Capabilities.Docker != nil {
		t.Errorf("Capabilities.Docker: got %v, want nil", got.Capabilities.Docker)
	}
	if len(got.AdditionalBindings) != 0 {
		t.Errorf("AdditionalBindings: got %v, want empty", got.AdditionalBindings)
	}
	if got.ContainerImage != "" {
		t.Errorf("ContainerImage: got %q, want empty", got.ContainerImage)
	}
}

func TestWorkspaceRepository_Save_UpsertOverwrites(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Save("ws", &WorkspaceMeta{HostCommands: []string{"old"}}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := repo.Save("ws", &WorkspaceMeta{HostCommands: []string{"new"}}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := repo.Load("ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalStringSlice(got.HostCommands, []string{"new"}) {
		t.Errorf("HostCommands after upsert: got %v, want [new]", got.HostCommands)
	}

	var count int
	if err := repo.conn.QueryRow(`SELECT COUNT(*) FROM workspaces WHERE slug = 'ws'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected a single row after upsert, got %d", count)
	}
}

func TestWorkspaceRepository_List_EmptyAndMultiple(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	slugs, err := repo.List()
	if err != nil {
		t.Fatalf("List (empty): %v", err)
	}
	if len(slugs) != 0 {
		t.Errorf("List (empty): got %v, want empty", slugs)
	}

	for _, slug := range []string{"beta", "alpha", "gamma"} {
		if err := repo.Save(slug, &WorkspaceMeta{}); err != nil {
			t.Fatalf("Save %q: %v", slug, err)
		}
	}

	slugs, err = repo.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"} // ORDER BY slug
	if !equalStringSlice(slugs, want) {
		t.Errorf("List: got %v, want %v", slugs, want)
	}
}

func TestWorkspaceRepository_EnsureDefault_Idempotent(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault (1st): %v", err)
	}
	got, err := repo.Load(DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("Load(default): %v", err)
	}
	if len(got.HostCommands) != 0 {
		t.Errorf("expected empty default workspace, got HostCommands=%v", got.HostCommands)
	}

	// Mutate it, then EnsureDefault again: existing content must survive
	// (EnsureDefault must not clobber user edits, same contract as the
	// yaml-backed WorkspaceStore.EnsureDefault).
	if err := repo.Save(DefaultWorkspaceSlug, &WorkspaceMeta{Env: map[string]string{"USER_SET": "yes"}}); err != nil {
		t.Fatalf("Save(default): %v", err)
	}
	if err := repo.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault (2nd): %v", err)
	}
	got, err = repo.Load(DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("Load(default) after 2nd EnsureDefault: %v", err)
	}
	if got.Env["USER_SET"] != "yes" {
		t.Errorf("EnsureDefault clobbered existing default workspace: Env=%v", got.Env)
	}
}

func TestWorkspaceRepository_Remove_RejectsDefault(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	if err := repo.Remove(DefaultWorkspaceSlug); err == nil {
		t.Fatal("Remove(default): expected error, got nil")
	}

	if _, err := repo.Load(DefaultWorkspaceSlug); err != nil {
		t.Errorf("default workspace was deleted despite rejected Remove: %v", err)
	}
}

func TestWorkspaceRepository_Remove_NotExist(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	err := repo.Remove("nonexistent")
	if err == nil {
		t.Fatal("Remove non-existent: expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove non-existent: error = %v, want os.ErrNotExist wrapped", err)
	}
}

// TestWorkspaceRepository_Remove_ReassignsProjectsToDefault pins the
// default workspace's implementation detail (docs/plans/workspace-db-consolidation.md
// 「default workspace の実装詳細」): removing a workspace that projects are
// assigned to must, in the same transaction, repoint those
// project_workspaces rows at 'default' rather than leaving them dangling
// or blocking the delete.
func TestWorkspaceRepository_Remove_ReassignsProjectsToDefault(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	if err := repo.Save("doomed", &WorkspaceMeta{HostCommands: []string{"gh"}}); err != nil {
		t.Fatalf("Save(doomed): %v", err)
	}

	if err := CreateProject(repo.conn, &Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := SetProjectWorkspace(repo.conn, "proj-1", "doomed"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	if err := repo.Remove("doomed"); err != nil {
		t.Fatalf("Remove(doomed): %v", err)
	}

	proj, err := GetProject(repo.conn, "proj-1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if proj.WorkspaceID != DefaultWorkspaceSlug {
		t.Errorf("proj-1.WorkspaceID after Remove(doomed) = %q, want %q", proj.WorkspaceID, DefaultWorkspaceSlug)
	}

	if _, err := repo.Load("doomed"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected 'doomed' workspace to be gone, got err=%v", err)
	}
}

func TestWorkspaceRepository_Load_InvalidSlug(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if _, err := repo.Load("Bad Slug"); err == nil {
		t.Error("Load with invalid slug: expected error, got nil")
	}
}

func TestWorkspaceRepository_Save_InvalidSlug(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Save("Bad Slug", &WorkspaceMeta{}); err == nil {
		t.Error("Save with invalid slug: expected error, got nil")
	}
}
