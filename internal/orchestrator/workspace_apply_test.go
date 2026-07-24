package orchestrator

import (
	"errors"
	"os"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

// newTestApplyDB returns a fresh in-memory *sql.DB with every migration
// applied, for tests exercising ApplyWorkspaceEnvelope directly (it needs a
// *sql.DB, not a *WorkspaceRepository, since it opens its own transaction).
func newTestApplyDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return d
}

func applyDoc(t *testing.T, slug string, fieldsPresent map[string]bool, spec WorkspaceEnvelopeSpec) *WorkspaceEnvelopeApply {
	t.Helper()
	return &WorkspaceEnvelopeApply{
		Envelope: &WorkspaceEnvelope{
			APIVersion: WorkspaceEnvelopeAPIVersion,
			Kind:       WorkspaceEnvelopeKind,
			Metadata:   WorkspaceEnvelopeMetadata{Name: slug},
			Spec:       spec,
		},
		FieldsPresent: fieldsPresent,
	}
}

func TestApplyWorkspaceEnvelope_CreatesNewWorkspace(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)

	doc := applyDoc(t, "team-a", map[string]bool{"host_commands": true, "env": true}, WorkspaceEnvelopeSpec{
		HostCommands: []string{"gh"},
		Env:          map[string]string{"FOO": "bar"},
	})
	result, err := ApplyWorkspaceEnvelope(d.Conn, doc, nil, false)
	if err != nil {
		t.Fatalf("ApplyWorkspaceEnvelope: %v", err)
	}
	if !result.Created {
		t.Error("Created = false, want true")
	}
	if !equalStrSlice(result.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("Meta.HostCommands = %v, want [gh]", result.Meta.HostCommands)
	}
	if result.Revision == "" {
		t.Error("Revision = \"\", want non-empty after a committed apply")
	}

	repo := NewWorkspaceRepository(d.Conn)
	meta, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("Load(team-a): %v", err)
	}
	if meta.Env["FOO"] != "bar" {
		t.Errorf("persisted Env[FOO] = %q, want bar", meta.Env["FOO"])
	}
}

func TestApplyWorkspaceEnvelope_MergeSemantics_AbsentFieldLeavesCurrentUntouched(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)
	repo := NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-b", &WorkspaceMeta{HostCommands: []string{"gh"}, Env: map[string]string{"FOO": "bar"}}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	// Only env present -- host_commands must survive untouched.
	doc := applyDoc(t, "team-b", map[string]bool{"env": true}, WorkspaceEnvelopeSpec{
		Env: map[string]string{"BAZ": "qux"},
	})
	result, err := ApplyWorkspaceEnvelope(d.Conn, doc, nil, false)
	if err != nil {
		t.Fatalf("ApplyWorkspaceEnvelope: %v", err)
	}
	if result.Created {
		t.Error("Created = true, want false (workspace already existed)")
	}
	if !equalStrSlice(result.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("Meta.HostCommands = %v, want preserved [gh]", result.Meta.HostCommands)
	}
	if result.Meta.Env["FOO"] != "" {
		t.Errorf("Meta.Env[FOO] = %q, want cleared (env field present, replaced wholesale)", result.Meta.Env["FOO"])
	}
	if result.Meta.Env["BAZ"] != "qux" {
		t.Errorf("Meta.Env[BAZ] = %q, want qux", result.Meta.Env["BAZ"])
	}
}

// TestApplyWorkspaceEnvelope_ProjectsPresentDetachesMissing pins Blocker 1's
// second half: when spec.projects is present (even as an empty/partial
// list), any project currently assigned to the workspace but absent from
// the new list must be detached (reassigned to DefaultWorkspaceSlug) — not
// just left alone.
func TestApplyWorkspaceEnvelope_ProjectsPresentDetachesMissing(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)
	repo := NewWorkspaceRepository(d.Conn)
	if err := repo.EnsureDefault(); err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	if err := repo.Save("team-c", &WorkspaceMeta{}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := CreateProject(d.Conn, &Project{ID: "proj-keep", WorkDir: "/tmp/proj-keep"}); err != nil {
		t.Fatalf("CreateProject(proj-keep): %v", err)
	}
	if err := CreateProject(d.Conn, &Project{ID: "proj-drop", WorkDir: "/tmp/proj-drop"}); err != nil {
		t.Fatalf("CreateProject(proj-drop): %v", err)
	}
	if err := SetProjectWorkspace(d.Conn, "proj-keep", "team-c"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-keep): %v", err)
	}
	if err := SetProjectWorkspace(d.Conn, "proj-drop", "team-c"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-drop): %v", err)
	}

	// spec.projects present, only proj-keep listed -- proj-drop must be detached.
	doc := applyDoc(t, "team-c", map[string]bool{"projects": true}, WorkspaceEnvelopeSpec{
		Projects: []WorkspaceEnvelopeProject{{Name: "keep"}},
	})
	result, err := ApplyWorkspaceEnvelope(d.Conn, doc, map[string]string{"keep": "proj-keep"}, false)
	if err != nil {
		t.Fatalf("ApplyWorkspaceEnvelope: %v", err)
	}
	if !equalStrSlice(result.AlreadyAttachedProjects, []string{"proj-keep"}) {
		t.Errorf("AlreadyAttachedProjects = %v, want [proj-keep]", result.AlreadyAttachedProjects)
	}
	if !equalStrSlice(result.DetachedProjects, []string{"proj-drop"}) {
		t.Errorf("DetachedProjects = %v, want [proj-drop]", result.DetachedProjects)
	}

	kept, err := GetProject(d.Conn, "proj-keep")
	if err != nil {
		t.Fatalf("GetProject(proj-keep): %v", err)
	}
	if kept.WorkspaceID != "team-c" {
		t.Errorf("proj-keep.WorkspaceID = %q, want team-c", kept.WorkspaceID)
	}
	dropped, err := GetProject(d.Conn, "proj-drop")
	if err != nil {
		t.Fatalf("GetProject(proj-drop): %v", err)
	}
	if dropped.WorkspaceID != DefaultWorkspaceSlug {
		t.Errorf("proj-drop.WorkspaceID = %q, want %q (detached to default)", dropped.WorkspaceID, DefaultWorkspaceSlug)
	}
}

// TestApplyWorkspaceEnvelope_ProjectsAbsentLeavesAssignmentsUntouched pins
// the "missing = don't touch" half of the same contract: a document with no
// spec.projects key at all must not detach anything.
func TestApplyWorkspaceEnvelope_ProjectsAbsentLeavesAssignmentsUntouched(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)
	repo := NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-d", &WorkspaceMeta{}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := CreateProject(d.Conn, &Project{ID: "proj-x", WorkDir: "/tmp/proj-x"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := SetProjectWorkspace(d.Conn, "proj-x", "team-d"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	doc := applyDoc(t, "team-d", map[string]bool{"env": true}, WorkspaceEnvelopeSpec{Env: map[string]string{"A": "b"}})
	result, err := ApplyWorkspaceEnvelope(d.Conn, doc, nil, false)
	if err != nil {
		t.Fatalf("ApplyWorkspaceEnvelope: %v", err)
	}
	if result.DetachedProjects != nil {
		t.Errorf("DetachedProjects = %v, want nil (spec.projects absent)", result.DetachedProjects)
	}

	proj, err := GetProject(d.Conn, "proj-x")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if proj.WorkspaceID != "team-d" {
		t.Errorf("proj-x.WorkspaceID = %q, want unchanged team-d", proj.WorkspaceID)
	}
}

// TestApplyWorkspaceEnvelope_DryRunPersistsNothing pins Major 1: a dry run
// runs through the real transaction (so it validates exactly what a real
// apply would) but must not leave anything committed.
func TestApplyWorkspaceEnvelope_DryRunPersistsNothing(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)

	doc := applyDoc(t, "team-e", map[string]bool{"host_commands": true}, WorkspaceEnvelopeSpec{HostCommands: []string{"gh"}})
	result, err := ApplyWorkspaceEnvelope(d.Conn, doc, nil, true)
	if err != nil {
		t.Fatalf("ApplyWorkspaceEnvelope dry-run: %v", err)
	}
	if !result.Created {
		t.Error("Created = false, want true (would-be create)")
	}
	if !equalStrSlice(result.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("Meta.HostCommands = %v, want [gh] (dry-run preview)", result.Meta.HostCommands)
	}
	// PR-1d codex round-2 Minor: a dry run must never report a revision —
	// the in-tx row it would have been read from is rolled back, so the
	// value would be an unusable ETag for a revision that was never
	// actually committed.
	if result.Revision != "" {
		t.Errorf("Revision = %q, want empty for a dry run", result.Revision)
	}

	repo := NewWorkspaceRepository(d.Conn)
	if _, err := repo.Load("team-e"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected team-e to NOT exist after a dry-run apply, got err=%v", err)
	}
}

// TestApplyWorkspaceEnvelope_RollsBackWholeTransactionOnAssignmentFailure
// pins Blocker 2's core atomicity guarantee: if the project-assignment half
// of an apply fails partway through, the workspace metadata write in the
// SAME call must not be left committed either.
func TestApplyWorkspaceEnvelope_RollsBackWholeTransactionOnAssignmentFailure(t *testing.T) {
	t.Parallel()
	d := newTestApplyDB(t)

	// "ghost-id" resolves to no projects row at all -- SetProjectWorkspace's
	// INSERT into project_workspaces(project_id, ...) then violates the
	// project_workspaces.project_id REFERENCES projects(id) foreign key
	// (PRAGMA foreign_keys=ON, internal/db/db.go), which is exactly the
	// class of "DB operation error mid-apply" Blocker 2 requires a full
	// rollback for.
	doc := applyDoc(t, "team-f", map[string]bool{"host_commands": true, "projects": true}, WorkspaceEnvelopeSpec{
		HostCommands: []string{"gh"},
		Projects:     []WorkspaceEnvelopeProject{{Name: "ghost"}},
	})
	_, err := ApplyWorkspaceEnvelope(d.Conn, doc, map[string]string{"ghost": "ghost-id"}, false)
	if err == nil {
		t.Fatal("expected an error from the foreign-key violation, got nil")
	}

	repo := NewWorkspaceRepository(d.Conn)
	if _, loadErr := repo.Load("team-f"); !errors.Is(loadErr, os.ErrNotExist) {
		t.Errorf("expected team-f's metadata write to have been rolled back too, got err=%v", loadErr)
	}
}
