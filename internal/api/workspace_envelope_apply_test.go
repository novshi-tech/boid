package api

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newTestWorkspacesConnDB returns a fresh in-memory *sql.DB with every
// migration applied, wired the same way ProjectAppService.WorkspacesConn is
// in production (internal/server/wire.go) — needed to exercise
// ProjectAppService.ApplyWorkspace/ExportWorkspaceEnvelopes, which open
// their own transactions rather than going through the narrower
// WorkspaceStore/ProjectRepository interfaces the rest of this package's
// tests stub out.
func newTestWorkspacesConnDB(t *testing.T) *db.DB {
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

func envelopeApplyDoc(slug string, fieldsPresent map[string]bool, spec orchestrator.WorkspaceEnvelopeSpec) *orchestrator.WorkspaceEnvelopeApply {
	return &orchestrator.WorkspaceEnvelopeApply{
		Envelope: &orchestrator.WorkspaceEnvelope{
			APIVersion: orchestrator.WorkspaceEnvelopeAPIVersion,
			Kind:       orchestrator.WorkspaceEnvelopeKind,
			Metadata:   orchestrator.WorkspaceEnvelopeMetadata{Name: slug},
			Spec:       spec,
		},
		FieldsPresent: fieldsPresent,
	}
}

// ---------------------------------------------------------------------------
// ApplyWorkspace: ambiguous project name refuses the whole apply
// (PR-1d codex round-2 Blocker 2)
// ---------------------------------------------------------------------------

// TestApplyWorkspace_AmbiguousProjectNameRefusesApply pins Blocker 2: two
// registered projects sharing the SAME project.yaml name ("api") must NOT
// both be silently detached when a document lists that name once — apply
// must refuse the whole operation with a clear ambiguity error instead, and
// must not touch the DB at all (no workspace row created, nothing
// committed).
func TestApplyWorkspace_AmbiguousProjectNameRefusesApply(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{
				{ID: "proj-1"},
				{ID: "proj-2"},
			},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				"proj-1": {ID: "proj-1", Name: "api"},
				"proj-2": {ID: "proj-2", Name: "api"},
			},
		},
		WorkspacesConn: d.Conn,
	}

	apply := envelopeApplyDoc("team-a", map[string]bool{"projects": true}, orchestrator.WorkspaceEnvelopeSpec{
		Projects: []orchestrator.WorkspaceEnvelopeProject{{Name: "api"}},
	})

	_, err := svc.ApplyWorkspace(apply, false)
	if err == nil {
		t.Fatal("expected an ambiguity error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if se.Code != http.StatusConflict {
		t.Errorf("Code = %d, want %d", se.Code, http.StatusConflict)
	}
	if !strings.Contains(se.Message, "ambiguous") || !strings.Contains(se.Message, "api") {
		t.Errorf("Message = %q, want it to mention the ambiguity and the name", se.Message)
	}
	// Both colliding IDs must be named so an operator can disambiguate.
	if !strings.Contains(se.Message, "proj-1") || !strings.Contains(se.Message, "proj-2") {
		t.Errorf("Message = %q, want it to name both colliding project IDs", se.Message)
	}

	// No side effects: the apply must have been refused before any DB
	// write, so team-a must not exist at all.
	if _, loadErr := orchestrator.NewWorkspaceRepository(d.Conn).Load("team-a"); !errors.Is(loadErr, os.ErrNotExist) {
		t.Errorf("expected team-a to NOT exist after a refused ambiguous apply, got err=%v", loadErr)
	}
}

// TestApplyWorkspace_UnambiguousNameStillWorks is the regression guard
// alongside the refusal test above: a name that resolves to exactly one
// project must still attach normally.
func TestApplyWorkspace_UnambiguousNameStillWorks(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{{ID: "proj-1"}},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				"proj-1": {ID: "proj-1", Name: "api"},
			},
		},
		WorkspacesConn: d.Conn,
	}

	apply := envelopeApplyDoc("team-a", map[string]bool{"projects": true}, orchestrator.WorkspaceEnvelopeSpec{
		Projects: []orchestrator.WorkspaceEnvelopeProject{{Name: "api"}},
	})

	result, err := svc.ApplyWorkspace(apply, false)
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	if len(result.AttachedProjects) != 1 || result.AttachedProjects[0] != "proj-1" {
		t.Errorf("AttachedProjects = %v, want [proj-1]", result.AttachedProjects)
	}
}

// ---------------------------------------------------------------------------
// ExportWorkspaceEnvelopes: ambiguous project name refuses export
// (PR-1d codex round-2 Blocker 2, export-side detection)
// ---------------------------------------------------------------------------

// TestExportWorkspaceEnvelopes_AmbiguousProjectNameRefusesExport pins the
// export-side half of Blocker 2: emitting an already-ambiguous
// spec.projects[] entry (one that a later `apply` could never unambiguously
// resolve back) is refused at export time instead.
func TestExportWorkspaceEnvelopes_AmbiguousProjectNameRefusesExport(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("CreateProject(proj-1): %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-2", WorkDir: "/tmp/proj-2"}); err != nil {
		t.Fatalf("CreateProject(proj-2): %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-1): %v", err)
	}
	// proj-2 is registered with the same name but assigned elsewhere (or
	// nowhere) — it still makes "api" ambiguous for a later ResolveProjectRef
	// call, so export must catch it too, not just same-workspace collisions.

	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{{ID: "proj-1"}, {ID: "proj-2"}},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				"proj-1": {ID: "proj-1", Name: "api"},
				"proj-2": {ID: "proj-2", Name: "api"},
			},
		},
		WorkspacesConn: d.Conn,
	}

	_, err := svc.ExportWorkspaceEnvelopes([]string{"team-a"})
	if err == nil {
		t.Fatal("expected an ambiguity error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if se.Code != http.StatusConflict {
		t.Errorf("Code = %d, want %d", se.Code, http.StatusConflict)
	}
	if !strings.Contains(se.Message, "ambiguous") {
		t.Errorf("Message = %q, want it to mention the ambiguity", se.Message)
	}
}

// TestExportWorkspaceEnvelopes_UsesSnapshotProjectsNotPostTxLookup pins
// Blocker 3: the exported project's fields (name/url) must come out intact
// via the snapshot's own transaction, with no separate post-transaction
// GetProject call needed.
func TestExportWorkspaceEnvelopes_UsesSnapshotProjectsNotPostTxLookup(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1", UpstreamURL: "https://example.com/proj-1.git"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{{ID: "proj-1"}},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				"proj-1": {ID: "proj-1", Name: "rook-server"},
			},
		},
		WorkspacesConn: d.Conn,
	}

	data, err := svc.ExportWorkspaceEnvelopes([]string{"team-a"})
	if err != nil {
		t.Fatalf("ExportWorkspaceEnvelopes: %v", err)
	}
	if !strings.Contains(string(data), "name: rook-server") {
		t.Errorf("export = %q, want it to contain the project name", data)
	}
	if !strings.Contains(string(data), "https://example.com/proj-1.git") {
		t.Errorf("export = %q, want it to contain the upstream URL", data)
	}
}
