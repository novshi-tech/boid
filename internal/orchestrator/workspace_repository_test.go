package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	if got.ContainerImage != meta.ContainerImage {
		t.Errorf("ContainerImage: got %q, want %q", got.ContainerImage, meta.ContainerImage)
	}
}

// TestWorkspaceRepository_Load_DiscardsStoredAdditionalBindingsAndWarns pins
// the Phase 4 PR4 (docs/plans/home-workspace-volume.md) regression contract
// for the DB-backed path: a `workspaces.additional_bindings` column value
// written by a binary that predates this PR (WorkspaceMeta still had the
// field then) must not surface on the WorkspaceMeta Load returns (the type
// has no field for it any more), must not error the Load, and must log a
// warning so an operator understands why a previously-working workspace
// bind mount silently stopped applying after the upgrade. The column is
// written directly via raw SQL here (bypassing Save/marshalWorkspaceMetaColumns,
// which — post-PR4 — can only ever write "[]") to simulate exactly that
// pre-PR4-written-row scenario.
func TestWorkspaceRepository_Load_DiscardsStoredAdditionalBindingsAndWarns(t *testing.T) {
	repo := newTestWorkspaceRepo(t)

	if err := repo.Create("legacy-ws", &WorkspaceMeta{Env: map[string]string{"FOO": "bar"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.conn.Exec(
		`UPDATE workspaces SET additional_bindings = ? WHERE slug = ?`,
		`[{"source":"/opt/volta","target":"/opt/volta","mode":"rw"}]`, "legacy-ws",
	); err != nil {
		t.Fatalf("seed legacy additional_bindings column: %v", err)
	}

	buf := captureSlog(t)
	got, err := repo.Load("legacy-ws")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want bar (rest of the row must still load)", got.Env["FOO"])
	}
	if !strings.Contains(buf.String(), "additional_bindings") {
		t.Errorf("expected a warning mentioning additional_bindings, got log: %s", buf.String())
	}

	// Saving the loaded meta back must zero out the stale column rather than
	// round-tripping a value WorkspaceMeta can no longer carry.
	if err := repo.Save("legacy-ws", got); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var rawBindingsJSON string
	if err := repo.conn.QueryRow(`SELECT additional_bindings FROM workspaces WHERE slug = ?`, "legacy-ws").Scan(&rawBindingsJSON); err != nil {
		t.Fatalf("query additional_bindings column: %v", err)
	}
	if rawBindingsJSON != "[]" {
		t.Errorf("additional_bindings column after Save = %q, want [] (Save must not resurrect a value WorkspaceMeta cannot carry)", rawBindingsJSON)
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

// --- Create (PR4, docs/plans/workspace-db-consolidation.md Step A) ---

func TestWorkspaceRepository_Create_InsertsNewRow(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	meta := &WorkspaceMeta{HostCommands: []string{"gh"}, Env: map[string]string{"FOO": "bar"}}
	if err := repo.Create("new-ws", meta); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Load("new-ws")
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

// TestWorkspaceRepository_Create_ConflictReturnsErrExist pins the 409
// contract (docs/plans/workspace-db-consolidation.md Step A/C): Create must
// be insert-only and reject an already-existing slug by wrapping
// os.ErrExist, so the API handler (POST /api/workspaces) can map it to HTTP
// 409 without string-matching the error message.
func TestWorkspaceRepository_Create_ConflictReturnsErrExist(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Create("dup", &WorkspaceMeta{HostCommands: []string{"first"}}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := repo.Create("dup", &WorkspaceMeta{HostCommands: []string{"second"}})
	if err == nil {
		t.Fatal("second Create: expected error, got nil")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("second Create: error = %v, want os.ErrExist wrapped", err)
	}

	// The original row must be untouched by the failed conflicting Create.
	got, loadErr := repo.Load("dup")
	if loadErr != nil {
		t.Fatalf("Load after conflict: %v", loadErr)
	}
	if !equalStringSlice(got.HostCommands, []string{"first"}) {
		t.Errorf("HostCommands after conflicting Create: got %v, want [first] (must not be clobbered)", got.HostCommands)
	}
}

func TestWorkspaceRepository_Create_InvalidSlug(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Create("Bad Slug", &WorkspaceMeta{}); err == nil {
		t.Error("Create with invalid slug: expected error, got nil")
	}
}

// TestWorkspaceRepository_Create_ConflictWithDefault verifies that Create
// against the always-present default workspace slug reports the same
// os.ErrExist conflict as any other pre-existing slug (rather than, say,
// silently upserting over it — that would defeat the point of a strict
// insert-only Create).
func TestWorkspaceRepository_Create_ConflictWithDefault(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	err := repo.Create(DefaultWorkspaceSlug, &WorkspaceMeta{})
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("Create(default): error = %v, want os.ErrExist wrapped", err)
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

// --- LoadWithRevision / UpdateIfRevisionMatches (MAJOR 1, codex review PR4:
// CAS化 — docs/plans/workspace-db-consolidation.md) ---

// TestWorkspaceRepository_LoadWithRevision_ReturnsMetaAndRevisionFromSameRow
// pins the atomic-snapshot contract: meta and revision must come from a
// single SELECT (one row), not two separate queries that could straddle a
// concurrent write.
func TestWorkspaceRepository_LoadWithRevision_ReturnsMetaAndRevisionFromSameRow(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Save("team-a", &WorkspaceMeta{HostCommands: []string{"gh"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	meta, revision, err := repo.LoadWithRevision("team-a")
	if err != nil {
		t.Fatalf("LoadWithRevision: %v", err)
	}
	if !equalStringSlice(meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", meta.HostCommands)
	}
	if revision == "" {
		t.Error("expected non-empty revision")
	}

	// The revision must match what GetWorkspaceSummary (the pre-existing,
	// separate-query path) reports for the same row.
	summary, err := GetWorkspaceSummary(repo.conn, "team-a")
	if err != nil {
		t.Fatalf("GetWorkspaceSummary: %v", err)
	}
	if revision != summary.Revision {
		t.Errorf("LoadWithRevision revision = %q, GetWorkspaceSummary revision = %q, want equal", revision, summary.Revision)
	}
}

func TestWorkspaceRepository_LoadWithRevision_NotExist(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	_, _, err := repo.LoadWithRevision("nonexistent")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist wrapped, got: %v", err)
	}
}

// TestWorkspaceRepository_UpdateIfRevisionMatches_Success pins the core CAS
// contract: an UPDATE with the correct expectedRevision succeeds in one SQL
// statement, bumps the revision, and persists the new meta.
func TestWorkspaceRepository_UpdateIfRevisionMatches_Success(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Save("team-a", &WorkspaceMeta{HostCommands: []string{"old"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, revision, err := repo.LoadWithRevision("team-a")
	if err != nil {
		t.Fatalf("LoadWithRevision: %v", err)
	}

	newRevision, matched, err := repo.UpdateIfRevisionMatches("team-a", revision, &WorkspaceMeta{HostCommands: []string{"new"}})
	if err != nil {
		t.Fatalf("UpdateIfRevisionMatches: %v", err)
	}
	if !matched {
		t.Fatal("expected matched=true for a correct revision")
	}
	if newRevision == "" || newRevision == revision {
		t.Errorf("newRevision = %q, want a fresh non-empty value distinct from %q", newRevision, revision)
	}

	got, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalStringSlice(got.HostCommands, []string{"new"}) {
		t.Errorf("HostCommands after update = %v, want [new]", got.HostCommands)
	}
}

// TestWorkspaceRepository_UpdateIfRevisionMatches_StaleRevisionNoOp pins the
// rejection path: a stale expectedRevision must not modify the row at all
// (matched=false, no partial write).
func TestWorkspaceRepository_UpdateIfRevisionMatches_StaleRevisionNoOp(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	if err := repo.Save("team-a", &WorkspaceMeta{HostCommands: []string{"old"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, matched, err := repo.UpdateIfRevisionMatches("team-a", "2000-01-01T00:00:00Z", &WorkspaceMeta{HostCommands: []string{"new"}})
	if err != nil {
		t.Fatalf("UpdateIfRevisionMatches: %v", err)
	}
	if matched {
		t.Fatal("expected matched=false for a stale revision")
	}

	got, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalStringSlice(got.HostCommands, []string{"old"}) {
		t.Errorf("HostCommands after rejected update = %v, want unchanged [old]", got.HostCommands)
	}
}

// TestWorkspaceRepository_UpdateIfRevisionMatches_NonexistentSlug pins that a
// slug with no row at all also reports matched=false (not a hard error) —
// the caller (ProjectAppService.UpdateWorkspace) distinguishes "gone" from
// "stale" by a subsequent existence check, not by an error type here.
func TestWorkspaceRepository_UpdateIfRevisionMatches_NonexistentSlug(t *testing.T) {
	t.Parallel()
	repo := newTestWorkspaceRepo(t)

	_, matched, err := repo.UpdateIfRevisionMatches("ghost", "2026-01-01T00:00:00Z", &WorkspaceMeta{})
	if err != nil {
		t.Fatalf("UpdateIfRevisionMatches: %v", err)
	}
	if matched {
		t.Fatal("expected matched=false for a nonexistent slug")
	}
}

// TestWorkspaceRepository_UpdateIfRevisionMatches_ConcurrentPUTsOnlyOneWins
// is the true-concurrency regression guard for MAJOR 1: two goroutines racing
// a CAS update against the same starting revision must not both succeed —
// exactly one call reports matched=true.
func TestWorkspaceRepository_UpdateIfRevisionMatches_ConcurrentPUTsOnlyOneWins(t *testing.T) {
	repo := newTestWorkspaceRepo(t)

	if err := repo.Save("team-a", &WorkspaceMeta{HostCommands: []string{"old"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, revision, err := repo.LoadWithRevision("team-a")
	if err != nil {
		t.Fatalf("LoadWithRevision: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	matchedCount := int32(0)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, matched, err := repo.UpdateIfRevisionMatches("team-a", revision, &WorkspaceMeta{HostCommands: []string{fmt.Sprintf("writer-%d", i)}})
			if err != nil {
				t.Errorf("UpdateIfRevisionMatches: %v", err)
				return
			}
			if matched {
				atomic.AddInt32(&matchedCount, 1)
			}
		}(i)
	}
	wg.Wait()

	if matchedCount != 1 {
		t.Errorf("matchedCount = %d, want exactly 1 (only one of %d concurrent CAS updates should win)", matchedCount, n)
	}
}
