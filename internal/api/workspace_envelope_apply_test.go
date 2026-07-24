package api

import (
	"errors"
	"fmt"
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

// ---------------------------------------------------------------------------
// PR-1d codex round-3 Blocker: name-vs-ID collision (destructive
// reassignment via ResolveProjectRef's ID-first precedence)
// ---------------------------------------------------------------------------

// TestApplyWorkspace_NameMatchingAnotherProjectIDResolvesToNameOwner pins the
// round-3 Blocker: project A (id=proj-a, name=proj-b) and project B
// (id=proj-b, name=other) are both registered, and A is already attached to
// team-a. Applying a document listing `name: proj-b` must resolve to A (the
// project whose project.yaml name actually IS "proj-b") via strict
// name-only matching — NOT to B via ResolveProjectRef's id-exact-match
// precedence, which would attach B and destructively detach A even though
// the document never mentioned B at all.
func TestApplyWorkspace_NameMatchingAnotherProjectIDResolvesToNameOwner(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-a", WorkDir: "/tmp/proj-a"}); err != nil {
		t.Fatalf("CreateProject(proj-a): %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-b", WorkDir: "/tmp/proj-b"}); err != nil {
		t.Fatalf("CreateProject(proj-b): %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-a", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-a): %v", err)
	}

	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{
				{ID: "proj-a"},
				{ID: "proj-b"},
			},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				"proj-a": {ID: "proj-a", Name: "proj-b"},
				"proj-b": {ID: "proj-b", Name: "other"},
			},
		},
		WorkspacesConn: d.Conn,
	}

	apply := envelopeApplyDoc("team-a", map[string]bool{"projects": true}, orchestrator.WorkspaceEnvelopeSpec{
		Projects: []orchestrator.WorkspaceEnvelopeProject{{Name: "proj-b"}},
	})

	result, err := svc.ApplyWorkspace(apply, false)
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	if len(result.AttachedProjects) != 0 {
		t.Errorf("AttachedProjects = %v, want empty (proj-a was already attached; proj-b must never be attached)", result.AttachedProjects)
	}
	if len(result.AlreadyAttachedProjects) != 1 || result.AlreadyAttachedProjects[0] != "proj-a" {
		t.Errorf("AlreadyAttachedProjects = %v, want [proj-a] (resolved by NAME, not by proj-b's ID)", result.AlreadyAttachedProjects)
	}
	if len(result.DetachedProjects) != 0 {
		t.Errorf("DetachedProjects = %v, want empty — proj-a must NOT be detached", result.DetachedProjects)
	}
}

// TestExportWorkspaceEnvelopes_NameMatchingAnotherProjectIDRefusesExport
// pins the export-side half of the round-3 Blocker: emitting proj-a's name
// ("proj-b") would be indistinguishable from proj-b's own identity on a
// later apply — export must refuse rather than produce a document that
// round-trips to the wrong project.
func TestExportWorkspaceEnvelopes_NameMatchingAnotherProjectIDRefusesExport(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-a", WorkDir: "/tmp/proj-a"}); err != nil {
		t.Fatalf("CreateProject(proj-a): %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-b", WorkDir: "/tmp/proj-b"}); err != nil {
		t.Fatalf("CreateProject(proj-b): %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-a", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-a): %v", err)
	}
	// proj-b need not be assigned to team-a at all — the collision makes
	// exporting proj-a's name unsafe regardless of where proj-b lives.

	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{
				{ID: "proj-a"},
				{ID: "proj-b"},
			},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				"proj-a": {ID: "proj-a", Name: "proj-b"},
				"proj-b": {ID: "proj-b", Name: "other"},
			},
		},
		WorkspacesConn: d.Conn,
	}

	_, err := svc.ExportWorkspaceEnvelopes([]string{"team-a"})
	if err == nil {
		t.Fatal("expected a name-vs-id collision error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if se.Code != http.StatusConflict {
		t.Errorf("Code = %d, want %d", se.Code, http.StatusConflict)
	}
	if !strings.Contains(se.Message, "proj-b") {
		t.Errorf("Message = %q, want it to mention the colliding id/name", se.Message)
	}
}

// TestExportWorkspaceEnvelopes_SelfConsistentNameEqualsOwnIDIsNotACollision
// is the regression guard alongside the two tests above: a project whose
// OWN name happens to equal its OWN id is not a collision — nothing else
// could be hijacked, since ResolveProjectRef's id-exact-match step would
// still land on the exact same project either way.
func TestExportWorkspaceEnvelopes_SelfConsistentNameEqualsOwnIDIsNotACollision(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-a", WorkDir: "/tmp/proj-a"}); err != nil {
		t.Fatalf("CreateProject(proj-a): %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-a", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-a): %v", err)
	}

	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{{ID: "proj-a"}},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				"proj-a": {ID: "proj-a", Name: "proj-a"},
			},
		},
		WorkspacesConn: d.Conn,
	}

	data, err := svc.ExportWorkspaceEnvelopes([]string{"team-a"})
	if err != nil {
		t.Fatalf("ExportWorkspaceEnvelopes: %v (want success — self-consistent name==id is not a collision)", err)
	}
	if !strings.Contains(string(data), "name: proj-a") {
		t.Errorf("export = %q, want it to contain the project name", data)
	}
}

// ---------------------------------------------------------------------------
// PR-1d codex round-3 Major: non-404 project resolution failures must abort
// apply, not be folded into MissingProjects
// ---------------------------------------------------------------------------

// TestApplyWorkspace_ProjectResolutionErrorAbortsApply pins the round-3
// Major: a transient repository failure (e.g. ListProjects returning 500)
// while resolving spec.projects[].name must abort the WHOLE apply with the
// underlying error, not be silently folded into "missing project" and let
// the apply proceed to (potentially) detach a currently-attached project on
// what may be a merely transient failure.
func TestApplyWorkspace_ProjectResolutionErrorAbortsApply(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			listErr: fmt.Errorf("boom: repository unavailable"),
		},
		Meta:           &stubProjectMetaStore{},
		WorkspacesConn: d.Conn,
	}

	apply := envelopeApplyDoc("team-a", map[string]bool{"projects": true}, orchestrator.WorkspaceEnvelopeSpec{
		Projects: []orchestrator.WorkspaceEnvelopeProject{{Name: "api"}},
	})

	_, err := svc.ApplyWorkspace(apply, false)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if se.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want %d", se.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(se.Message, "boom") {
		t.Errorf("Message = %q, want it to mention the underlying repository error", se.Message)
	}

	// No side effects: the apply must have been refused before any DB
	// write, so team-a must not exist at all.
	if _, loadErr := orchestrator.NewWorkspaceRepository(d.Conn).Load("team-a"); !errors.Is(loadErr, os.ErrNotExist) {
		t.Errorf("expected team-a to NOT exist after an aborted apply, got err=%v", loadErr)
	}
}

// ---------------------------------------------------------------------------
// PR-1d codex round-4 Blocker: unavailable cached project metadata must
// refuse export/apply rather than fall back to a basename / silently
// translate into a 404 miss.
//
// Concrete failure chain codex round-4 flagged: (1) ProjectStore.LoadAll can
// fail to hydrate a project (project.yaml unreadable), leaving cached
// Meta.Name empty while the DB row + workspace assignment are untouched;
// (2) projectCollisions used to skip empty-name projects from collision
// detection; (3) export used to fall back to the work-directory basename
// when Meta.Name was empty; (4) apply's resolveProjectByNameExact folds an
// unfindable name into a 404 "missing" — reapplying the fallback-name
// export would then detach the real project (or attach the wrong one, if
// the fallback collided with another project's real name).
// ---------------------------------------------------------------------------

// TestExportWorkspaceEnvelopes_UnavailableProjectNameRefusesExport pins the
// export-side refusal: a project assigned to an exported workspace whose
// Meta.Name is empty (project.yaml could not be hydrated) must refuse the
// WHOLE export with a clear error — never fall back to the work_dir
// basename, and never emit partial output (docs for OTHER, healthy
// workspaces must not be returned either).
func TestExportWorkspaceEnvelopes_UnavailableProjectNameRefusesExport(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("Save(team-a): %v", err)
	}
	if err := repo.Save("team-b", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("Save(team-b): %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-broken", WorkDir: "/tmp/some-basename"}); err != nil {
		t.Fatalf("CreateProject(proj-broken): %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-healthy", WorkDir: "/tmp/proj-healthy"}); err != nil {
		t.Fatalf("CreateProject(proj-healthy): %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-broken", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-broken): %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-healthy", "team-b"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-healthy): %v", err)
	}

	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{{ID: "proj-broken"}, {ID: "proj-healthy"}},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				// proj-broken intentionally has NO entry: simulates
				// ProjectStore.LoadAll failing to hydrate its project.yaml,
				// leaving Meta.Name empty.
				"proj-healthy": {ID: "proj-healthy", Name: "healthy"},
			},
		},
		WorkspacesConn: d.Conn,
	}

	data, err := svc.ExportWorkspaceEnvelopes(nil)
	if err == nil {
		t.Fatalf("expected a refusal error, got nil (data: %s)", data)
	}
	if data != nil {
		t.Errorf("expected no partial output on refusal, got %q", data)
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if se.Code != http.StatusConflict {
		t.Errorf("Code = %d, want %d", se.Code, http.StatusConflict)
	}
	if !strings.Contains(se.Message, "proj-broken") || !strings.Contains(se.Message, "no authoritative name") {
		t.Errorf("Message = %q, want it to name the project and explain it has no authoritative name", se.Message)
	}
	// Must NOT have fallen back to the work_dir basename anywhere in the
	// error (there is nothing to compare it against once refused, but the
	// basename must not leak into a success path either — pinned by data
	// being nil above).
	if strings.Contains(se.Message, "some-basename") {
		t.Errorf("Message = %q, must not reference the work_dir basename fallback", se.Message)
	}
}

// TestApplyWorkspace_UnavailableProjectNameRefusesApply pins the apply-side
// refusal: when ANY registered project's Meta.Name is unavailable
// (project.yaml could not be hydrated), an apply that touches
// spec.projects must refuse outright — never translate the unresolvable
// name into resolveProjectByNameExact's ordinary 404 "not registered yet"
// path, which would let a currently-attached-but-unhydratable project be
// silently detached.
func TestApplyWorkspace_UnavailableProjectNameRefusesApply(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("CreateProject(proj-1): %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-broken", WorkDir: "/tmp/proj-broken"}); err != nil {
		t.Fatalf("CreateProject(proj-broken): %v", err)
	}
	// proj-broken is currently attached to team-a — exactly the project a
	// naive "unfindable name == missing" fold would silently detach.
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-broken", "team-a"); err != nil {
		t.Fatalf("SetProjectWorkspace(proj-broken): %v", err)
	}

	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{
				{ID: "proj-1"},
				{ID: "proj-broken"},
			},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{
				"proj-1": {ID: "proj-1", Name: "api"},
				// proj-broken intentionally has NO entry: its project.yaml
				// could not be hydrated, leaving Meta.Name empty.
			},
		},
		WorkspacesConn: d.Conn,
	}

	apply := envelopeApplyDoc("team-a", map[string]bool{"projects": true}, orchestrator.WorkspaceEnvelopeSpec{
		Projects: []orchestrator.WorkspaceEnvelopeProject{{Name: "api"}},
	})

	_, err := svc.ApplyWorkspace(apply, false)
	if err == nil {
		t.Fatal("expected a refusal error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if se.Code != http.StatusConflict {
		t.Errorf("Code = %d, want %d", se.Code, http.StatusConflict)
	}
	if !strings.Contains(se.Message, "proj-broken") || !strings.Contains(se.Message, "no authoritative name") {
		t.Errorf("Message = %q, want it to name the unresolvable project and explain it has no authoritative name", se.Message)
	}

	// No side effects: the apply must have been refused before any DB
	// write, so team-a must not exist at all (and proj-broken must remain
	// exactly where it was, though there's no DB row for team-a to check
	// that against here since it was never created).
	if _, loadErr := orchestrator.NewWorkspaceRepository(d.Conn).Load("team-a"); !errors.Is(loadErr, os.ErrNotExist) {
		t.Errorf("expected team-a to NOT exist after a refused apply, got err=%v", loadErr)
	}
}

// TestApplyWorkspace_UnavailableProjectNameRefusalHappensBeforeDBWrite is a
// stricter no-side-effects guard than the test above: it seeds team-a with
// existing metadata via a real (non-refused) apply first, then attempts a
// second apply that should be refused due to an unavailable project name,
// and asserts team-a's metadata is byte-for-byte unchanged (not just
// "doesn't exist").
func TestApplyWorkspace_UnavailableProjectNameRefusalHappensBeforeDBWrite(t *testing.T) {
	d := newTestWorkspacesConnDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-broken", WorkDir: "/tmp/proj-broken"}); err != nil {
		t.Fatalf("CreateProject(proj-broken): %v", err)
	}

	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{{ID: "proj-broken"}},
		},
		Meta: &stubProjectMetaStore{
			metas: map[string]*orchestrator.ProjectMeta{},
		},
		WorkspacesConn: d.Conn,
	}

	// Seed team-a via a projects-untouched apply (succeeds: FieldsPresent
	// does not include "projects", so the unavailable-name check never
	// runs for this call).
	seed := envelopeApplyDoc("team-a", map[string]bool{}, orchestrator.WorkspaceEnvelopeSpec{})
	if _, err := svc.ApplyWorkspace(seed, false); err != nil {
		t.Fatalf("seed ApplyWorkspace: %v", err)
	}

	_, beforeRevision, err := orchestrator.NewWorkspaceRepository(d.Conn).LoadWithRevision("team-a")
	if err != nil {
		t.Fatalf("LoadWithRevision(team-a) after seed: %v", err)
	}

	apply := envelopeApplyDoc("team-a", map[string]bool{"projects": true}, orchestrator.WorkspaceEnvelopeSpec{
		Projects: []orchestrator.WorkspaceEnvelopeProject{{Name: "anything"}},
	})
	if _, err := svc.ApplyWorkspace(apply, false); err == nil {
		t.Fatal("expected a refusal error, got nil")
	}

	_, afterRevision, err := orchestrator.NewWorkspaceRepository(d.Conn).LoadWithRevision("team-a")
	if err != nil {
		t.Fatalf("LoadWithRevision(team-a) after refused apply: %v", err)
	}
	if beforeRevision != afterRevision {
		t.Errorf("team-a Revision changed from %q to %q — refused apply must not write to the DB at all", beforeRevision, afterRevision)
	}
}
