package api

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubWorkspaceStore implements the WorkspaceStore interface (Load / Save /
// Create / Remove) against an in-memory map, so ProjectAppService's
// workspace CRUD methods (docs/plans/workspace-db-consolidation.md PR4
// Step C/D/E/F) can be unit-tested without a real SQLite DB.
type stubWorkspaceStore struct {
	metas map[string]*orchestrator.WorkspaceMeta

	loadErr   error
	saveErr   error
	createErr error
	removeErr error

	saveCalls   []string
	removeCalls []string

	// revisions backs LoadWithRevision/UpdateIfRevisionMatches (MAJOR 1).
	revisions map[string]string

	// loadHook and createHook, if set, are called synchronously at the
	// start of Load/Create respectively (MAJOR 1, codex review round 2,
	// docs/plans/workspace-db-consolidation.md). Used by
	// TestUpdateWorkspace_ForcePathBlocksMidDelete and
	// TestCreateWorkspace_BlocksMidRemove to pause a mutation mid-flight
	// (while ProjectAppService.mu is held) so a concurrent RemoveWorkspace
	// call can be started and observed blocking on that same mutex.
	loadHook   func(slug string)
	createHook func(slug string)
}

func newStubWorkspaceStore() *stubWorkspaceStore {
	return &stubWorkspaceStore{metas: map[string]*orchestrator.WorkspaceMeta{}}
}

func (s *stubWorkspaceStore) Load(slug string) (*orchestrator.WorkspaceMeta, error) {
	if s.loadHook != nil {
		s.loadHook(slug)
	}
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	meta, ok := s.metas[slug]
	if !ok {
		return nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
	}
	return meta, nil
}

func (s *stubWorkspaceStore) Save(slug string, meta *orchestrator.WorkspaceMeta) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saveCalls = append(s.saveCalls, slug)
	s.metas[slug] = meta
	return nil
}

func (s *stubWorkspaceStore) Create(slug string, meta *orchestrator.WorkspaceMeta) error {
	if s.createHook != nil {
		s.createHook(slug)
	}
	if s.createErr != nil {
		return s.createErr
	}
	if _, ok := s.metas[slug]; ok {
		return fmt.Errorf("workspace %q: %w", slug, os.ErrExist)
	}
	s.metas[slug] = meta
	return nil
}

func (s *stubWorkspaceStore) Remove(slug string) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	if _, ok := s.metas[slug]; !ok {
		return fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
	}
	s.removeCalls = append(s.removeCalls, slug)
	delete(s.metas, slug)
	return nil
}

// revisions backs LoadWithRevision/UpdateIfRevisionMatches (MAJOR 1, codex
// review): a slug's "current revision" for CAS purposes. Tests that never
// call these two methods can leave it nil/unpopulated.
func (s *stubWorkspaceStore) revisionFor(slug string) string {
	if s.revisions == nil {
		return "rev-0"
	}
	if r, ok := s.revisions[slug]; ok {
		return r
	}
	return "rev-0"
}

func (s *stubWorkspaceStore) LoadWithRevision(slug string) (*orchestrator.WorkspaceMeta, string, error) {
	meta, err := s.Load(slug)
	if err != nil {
		return nil, "", err
	}
	return meta, s.revisionFor(slug), nil
}

func (s *stubWorkspaceStore) UpdateIfRevisionMatches(slug string, expectedRevision string, meta *orchestrator.WorkspaceMeta) (string, bool, error) {
	if _, ok := s.metas[slug]; !ok {
		return "", false, nil
	}
	if s.revisionFor(slug) != expectedRevision {
		return "", false, nil
	}
	if s.saveErr != nil {
		return "", false, s.saveErr
	}
	s.saveCalls = append(s.saveCalls, slug)
	s.metas[slug] = meta
	newRev := s.revisionFor(slug) + "+1"
	if s.revisions == nil {
		s.revisions = map[string]string{}
	}
	s.revisions[slug] = newRev
	return newRev, true, nil
}

// newWorkspaceTestService wires a ProjectAppService with the given
// workspace store and a repository whose GetWorkspaceSummary/ListProjects
// answer from summaries/projects.
func newWorkspaceTestService(ws *stubWorkspaceStore, summaries map[string]*orchestrator.WorkspaceSummary, projects []*orchestrator.Project) *ProjectAppService {
	return &ProjectAppService{
		Projects: &stubProjectRepository{
			projects:           projects,
			workspaceSummaries: summaries,
		},
		Meta:       &stubProjectMetaStore{},
		Workspaces: ws,
	}
}

func TestProjectAppService_CreateWorkspace_Success(t *testing.T) {
	ws := newStubWorkspaceStore()
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", ProjectCount: 0, Revision: "2026-01-01T00:00:00Z"},
	}
	svc := newWorkspaceTestService(ws, summaries, nil)

	meta := &orchestrator.WorkspaceMeta{HostCommands: []string{"gh"}}
	detail, err := svc.CreateWorkspace("team-a", meta)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if detail.Slug != "team-a" {
		t.Errorf("Slug = %q, want team-a", detail.Slug)
	}
	if detail.Revision != "2026-01-01T00:00:00Z" {
		t.Errorf("Revision = %q", detail.Revision)
	}
	if len(ws.metas["team-a"].HostCommands) != 1 {
		t.Errorf("workspace store did not receive Create: %+v", ws.metas)
	}
}

// --- HostCommands reference validation (MAJOR 2, codex review:
// docs/plans/workspace-db-consolidation.md) ---

// TestCreateWorkspace_RejectsUnknownHostCommandRef pins MAJOR 2: a
// meta.HostCommands reference with no corresponding entry in the daemon's
// live aggregated snapshot must be rejected with 400 at write time, rather
// than silently persisted and only warned-about + skipped at dispatch.
func TestCreateWorkspace_RejectsUnknownHostCommandRef(t *testing.T) {
	ws := newStubWorkspaceStore()
	svc := newWorkspaceTestService(ws, nil, nil)
	svc.HostCommands = func() map[string]orchestrator.HostCommandSpec {
		return map[string]orchestrator.HostCommandSpec{"gh": {}}
	}

	_, err := svc.CreateWorkspace("team-a", &orchestrator.WorkspaceMeta{HostCommands: []string{"unknown"}})
	if err == nil {
		t.Fatal("expected error for unknown host_commands reference, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
	if _, exists := ws.metas["team-a"]; exists {
		t.Error("workspace must not be persisted when host_commands validation fails")
	}
}

// TestUpdateWorkspace_RejectsUnknownHostCommandRef is the PUT-side
// counterpart of the test above.
func TestUpdateWorkspace_RejectsUnknownHostCommandRef(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	ws.revisions = map[string]string{"team-a": "rev-1"}
	svc := newWorkspaceTestService(ws, map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", Revision: "rev-1"},
	}, nil)
	svc.HostCommands = func() map[string]orchestrator.HostCommandSpec {
		return map[string]orchestrator.HostCommandSpec{"gh": {}}
	}

	_, err := svc.UpdateWorkspace("team-a", &orchestrator.WorkspaceMeta{HostCommands: []string{"unknown"}}, "rev-1", false)
	if err == nil {
		t.Fatal("expected error for unknown host_commands reference, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
	if len(ws.metas["team-a"].HostCommands) != 0 {
		t.Error("workspace must not be updated when host_commands validation fails")
	}
}

// TestCreateWorkspace_AcceptsKnownHostCommandRef is the regression guard
// alongside the rejection test: a name present in the live snapshot must
// still be accepted normally.
func TestCreateWorkspace_AcceptsKnownHostCommandRef(t *testing.T) {
	ws := newStubWorkspaceStore()
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", Revision: "rev-1"},
	}
	svc := newWorkspaceTestService(ws, summaries, nil)
	svc.HostCommands = func() map[string]orchestrator.HostCommandSpec {
		return map[string]orchestrator.HostCommandSpec{"gh": {}}
	}

	_, err := svc.CreateWorkspace("team-a", &orchestrator.WorkspaceMeta{HostCommands: []string{"gh"}})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if !equalStringSliceForTest(ws.metas["team-a"].HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", ws.metas["team-a"].HostCommands)
	}
}

// TestCreateWorkspace_AcceptsKitDerivedHostCommand pins the "validate after
// expansion" ordering MAJOR 2 requires: a host_commands name that only
// appears after MaterializeWorkspaceKitsForPersist expands meta.Kits (i.e.
// it is not in the request body's own HostCommands list) must still be
// recognized, as long as the daemon's live snapshot already knows about it
// (e.g. because the kit was aggregated into ~/.config/boid/host_commands.yaml
// at daemon startup or the last `boid host-commands reload`).
func TestCreateWorkspace_AcceptsKitDerivedHostCommand(t *testing.T) {
	kitsDir := t.TempDir()
	kitDir := kitsDir + "/toolkit"
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit dir: %v", err)
	}
	if err := os.WriteFile(kitDir+"/kit.yaml", []byte("host_commands:\n  toolcmd:\n    allow: [\"*\"]\n"), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}

	ws := newStubWorkspaceStore()
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", Revision: "rev-1"},
	}
	svc := newWorkspaceTestService(ws, summaries, nil)
	svc.KitsDir = kitsDir
	// Simulates the daemon's live snapshot already knowing about the kit's
	// host_command (aggregated from every installed kit.yaml at startup —
	// see internal/server/wire.go's buildProjectStore).
	svc.HostCommands = func() map[string]orchestrator.HostCommandSpec {
		return map[string]orchestrator.HostCommandSpec{"toolcmd": {}}
	}

	meta := &orchestrator.WorkspaceMeta{Kits: []string{"toolkit"}}
	detail, err := svc.CreateWorkspace("team-a", meta)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if !equalStringSliceForTest(detail.Meta.HostCommands, []string{"toolcmd"}) {
		t.Errorf("Meta.HostCommands = %v, want [toolcmd] (kit-derived, materialized then validated)", detail.Meta.HostCommands)
	}
}

// TestProjectAppService_CreateWorkspace_MaterializesKits pins the fix for a
// real e2e regression found while implementing PR4: the workspaces table
// has no `kits` column at all, so a body carrying a legacy Kits list (e.g.
// relayed verbatim by `boid workspace assign`'s auto-create from an old
// workspace.yaml) would silently lose the kit's env/host_commands/bindings
// contribution unless CreateWorkspace expands it first
// (orchestrator.MaterializeWorkspaceKitsForPersist).
func TestProjectAppService_CreateWorkspace_MaterializesKits(t *testing.T) {
	kitsDir := t.TempDir()
	kitDir := kitsDir + "/toolkit"
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit dir: %v", err)
	}
	if err := os.WriteFile(kitDir+"/kit.yaml", []byte("env:\n  KIT_VAR: from-kit\n"), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}

	ws := newStubWorkspaceStore()
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", Revision: "rev-1"},
	}
	svc := newWorkspaceTestService(ws, summaries, nil)
	svc.KitsDir = kitsDir

	meta := &orchestrator.WorkspaceMeta{Kits: []string{"toolkit"}}
	detail, err := svc.CreateWorkspace("team-a", meta)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if detail.Meta.Env["KIT_VAR"] != "from-kit" {
		t.Errorf("Env[KIT_VAR] = %q, want from-kit (kit not materialized)", detail.Meta.Env["KIT_VAR"])
	}
	if len(detail.Meta.Kits) != 0 {
		t.Errorf("Kits = %v, want empty (materialized then cleared)", detail.Meta.Kits)
	}
	// The persisted store must carry the materialized result too, not just
	// the returned detail.
	if ws.metas["team-a"].Env["KIT_VAR"] != "from-kit" {
		t.Errorf("persisted Env[KIT_VAR] = %q, want from-kit", ws.metas["team-a"].Env["KIT_VAR"])
	}
}

func TestProjectAppService_CreateWorkspace_RejectsInvalidSlug(t *testing.T) {
	svc := newWorkspaceTestService(newStubWorkspaceStore(), nil, nil)
	_, err := svc.CreateWorkspace("Bad Slug", &orchestrator.WorkspaceMeta{})
	if err == nil {
		t.Fatal("expected error for invalid slug, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
}

func TestProjectAppService_CreateWorkspace_ConflictReturns409(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	svc := newWorkspaceTestService(ws, nil, nil)

	_, err := svc.CreateWorkspace("team-a", &orchestrator.WorkspaceMeta{})
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

func TestProjectAppService_GetWorkspace_Success(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{HostCommands: []string{"gh"}}
	// GetWorkspace reads the revision from LoadWithRevision (MAJOR 1), not
	// GetWorkspaceSummary — set both so this test keeps exercising the real
	// path regardless of which one the current implementation consults.
	ws.revisions = map[string]string{"team-a": "rev-1"}
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", ProjectCount: 1, Revision: "rev-1"},
	}
	projects := []*orchestrator.Project{
		{ID: "proj-1", WorkspaceID: "team-a"},
		{ID: "proj-2", WorkspaceID: "other"},
	}
	svc := newWorkspaceTestService(ws, summaries, projects)

	detail, err := svc.GetWorkspace("team-a")
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if detail.Revision != "rev-1" {
		t.Errorf("Revision = %q, want rev-1", detail.Revision)
	}
	if detail.ProjectCount != 1 {
		t.Errorf("ProjectCount = %d, want 1", detail.ProjectCount)
	}
	if len(detail.AssignedProjects) != 1 || detail.AssignedProjects[0] != "proj-1" {
		t.Errorf("AssignedProjects = %v, want [proj-1]", detail.AssignedProjects)
	}
	if !equalStringSliceForTest(detail.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("Meta.HostCommands = %v", detail.Meta.HostCommands)
	}
}

// TestGetWorkspace_ReturnsConsistentSnapshot pins MAJOR 1 (codex review,
// docs/plans/workspace-db-consolidation.md): GET /api/workspaces/{slug}
// previously read meta (WorkspaceStore.Load) and revision
// (ProjectRepository.GetWorkspaceSummary) via two separate queries, which
// could straddle a concurrent PUT and return a meta/revision pair that
// never coexisted in the DB. GetWorkspace must instead read both from a
// single atomic snapshot (WorkspaceStore.LoadWithRevision) — this test
// asserts that contract by checking the returned Meta/Revision came from
// LoadWithRevision's own bookkeeping, AND that GetWorkspaceSummary (the
// old, separate-query path) is never called at all during GetWorkspace.
func TestGetWorkspace_ReturnsConsistentSnapshot(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{HostCommands: []string{"gh"}}
	ws.revisions = map[string]string{"team-a": "rev-42"}
	repo := &stubProjectRepository{}
	svc := &ProjectAppService{Projects: repo, Meta: &stubProjectMetaStore{}, Workspaces: ws}

	detail, err := svc.GetWorkspace("team-a")
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if detail.Revision != "rev-42" {
		t.Errorf("Revision = %q, want rev-42 (from LoadWithRevision)", detail.Revision)
	}
	if !equalStringSliceForTest(detail.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("Meta.HostCommands = %v", detail.Meta.HostCommands)
	}
	if repo.getWorkspaceSummaryCalls != 0 {
		t.Errorf("GetWorkspaceSummary was called %d times, want 0 (GetWorkspace must use the atomic LoadWithRevision path exclusively)", repo.getWorkspaceSummaryCalls)
	}
}

func TestProjectAppService_GetWorkspace_NotFound(t *testing.T) {
	svc := newWorkspaceTestService(newStubWorkspaceStore(), nil, nil)
	_, err := svc.GetWorkspace("ghost")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected 404 StatusError, got %v", err)
	}
}

func TestProjectAppService_UpdateWorkspace_RequiresIfMatchWithoutForce(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", Revision: "rev-1"},
	}
	svc := newWorkspaceTestService(ws, summaries, nil)

	_, err := svc.UpdateWorkspace("team-a", &orchestrator.WorkspaceMeta{}, "", false)
	if err == nil {
		t.Fatal("expected error for missing If-Match, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusPreconditionRequired {
		t.Fatalf("expected 428 StatusError, got %v", err)
	}
}

func TestProjectAppService_UpdateWorkspace_MismatchedIfMatch(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", Revision: "rev-1"},
	}
	svc := newWorkspaceTestService(ws, summaries, nil)

	_, err := svc.UpdateWorkspace("team-a", &orchestrator.WorkspaceMeta{}, "rev-stale", false)
	if err == nil {
		t.Fatal("expected error for stale If-Match, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 StatusError, got %v", err)
	}
}

func TestProjectAppService_UpdateWorkspace_MatchingIfMatchSucceeds(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	// UpdateWorkspace's non-force path checks the revision via
	// UpdateIfRevisionMatches (MAJOR 1), not GetWorkspaceSummary.
	ws.revisions = map[string]string{"team-a": "rev-1"}
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", Revision: "rev-1"},
	}
	svc := newWorkspaceTestService(ws, summaries, nil)

	newMeta := &orchestrator.WorkspaceMeta{HostCommands: []string{"aws"}}
	detail, err := svc.UpdateWorkspace("team-a", newMeta, "rev-1", false)
	if err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if !equalStringSliceForTest(detail.Meta.HostCommands, []string{"aws"}) {
		t.Errorf("Meta.HostCommands = %v", detail.Meta.HostCommands)
	}
	if len(ws.saveCalls) != 1 || ws.saveCalls[0] != "team-a" {
		t.Errorf("expected exactly one Save(team-a), got %v", ws.saveCalls)
	}
}

func TestProjectAppService_UpdateWorkspace_ForceSkipsIfMatch(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	summaries := map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a", Revision: "rev-1"},
	}
	svc := newWorkspaceTestService(ws, summaries, nil)

	_, err := svc.UpdateWorkspace("team-a", &orchestrator.WorkspaceMeta{}, "", true)
	if err != nil {
		t.Fatalf("UpdateWorkspace with force: %v", err)
	}
}

func TestProjectAppService_UpdateWorkspace_NotFound(t *testing.T) {
	svc := newWorkspaceTestService(newStubWorkspaceStore(), nil, nil)
	_, err := svc.UpdateWorkspace("ghost", &orchestrator.WorkspaceMeta{}, "", true)
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected 404 StatusError, got %v", err)
	}
}

func TestProjectAppService_RemoveWorkspace_Success(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	svc := newWorkspaceTestService(ws, nil, nil)

	if err := svc.RemoveWorkspace("team-a"); err != nil {
		t.Fatalf("RemoveWorkspace: %v", err)
	}
	if _, ok := ws.metas["team-a"]; ok {
		t.Error("expected team-a to be removed from the store")
	}
}

func TestProjectAppService_RemoveWorkspace_RejectsDefault(t *testing.T) {
	svc := newWorkspaceTestService(newStubWorkspaceStore(), nil, nil)
	err := svc.RemoveWorkspace(orchestrator.DefaultWorkspaceSlug)
	if err == nil {
		t.Fatal("expected error removing default workspace, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
}

// recordingMetaStore wraps stubProjectMetaStore, additionally recording
// every SetWorkspaceID call so tests can assert on the in-memory cache-sync
// behavior RemoveWorkspace performs (see its doc comment: the DB-level
// re-assign transaction has no way to reach ProjectStore's cache itself).
type recordingMetaStore struct {
	stubProjectMetaStore
	setWorkspaceIDCalls map[string]string // project_id -> workspace_id
}

func newRecordingMetaStore() *recordingMetaStore {
	return &recordingMetaStore{setWorkspaceIDCalls: map[string]string{}}
}

func (s *recordingMetaStore) SetWorkspaceID(projectID, workspaceID string) {
	s.setWorkspaceIDCalls[projectID] = workspaceID
}

// TestProjectAppService_RemoveWorkspace_SyncsInMemoryCacheForReassignedProjects
// pins the cache-staleness fix: WorkspaceRepository.Remove reassigns
// projects to default at the DB layer inside its own transaction, but that
// has no way to reach the daemon's in-memory ProjectStore cache — without
// RemoveWorkspace explicitly mirroring the reassignment via
// Meta.SetWorkspaceID, GetWithWorkspace would keep hydrating affected
// projects off a stale workspace_id that no longer has a corresponding row
// (discovered via TestWorkspaceAPI_CreateShowUpdateRemove's real-daemon
// integration test logging an unexpected "workspace.yaml not found;
// running in degraded mode" warning after a workspace removal).
func TestProjectAppService_RemoveWorkspace_SyncsInMemoryCacheForReassignedProjects(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	meta := newRecordingMetaStore()
	svc := &ProjectAppService{
		Projects: &stubProjectRepository{
			projects: []*orchestrator.Project{
				{ID: "proj-1", WorkspaceID: "team-a"},
				{ID: "proj-2", WorkspaceID: "team-a"},
				{ID: "proj-3", WorkspaceID: "other"},
			},
		},
		Meta:       meta,
		Workspaces: ws,
	}

	if err := svc.RemoveWorkspace("team-a"); err != nil {
		t.Fatalf("RemoveWorkspace: %v", err)
	}

	if got := meta.setWorkspaceIDCalls["proj-1"]; got != orchestrator.DefaultWorkspaceSlug {
		t.Errorf("proj-1 SetWorkspaceID = %q, want %q", got, orchestrator.DefaultWorkspaceSlug)
	}
	if got := meta.setWorkspaceIDCalls["proj-2"]; got != orchestrator.DefaultWorkspaceSlug {
		t.Errorf("proj-2 SetWorkspaceID = %q, want %q", got, orchestrator.DefaultWorkspaceSlug)
	}
	if _, called := meta.setWorkspaceIDCalls["proj-3"]; called {
		t.Error("proj-3 was never assigned to team-a; SetWorkspaceID should not have been called for it")
	}
}

// TestRemoveWorkspaceAndAssign_RaceOnCacheSync pins MAJOR 3 (codex review,
// docs/plans/workspace-db-consolidation.md): RemoveWorkspace snapshots
// assigned projects, removes the workspace, then loops over that snapshot
// updating the in-memory cache to the default workspace. Without
// serialization, a concurrent SetProjectWorkspace reassigning one of those
// same projects to a *different*, still-existing workspace could have its
// cache write clobbered by RemoveWorkspace's stale-snapshot loop landing
// after it — leaving the cache pointing at "default" while the DB (and any
// fresh GetWithWorkspace hydration) says the project belongs to the new
// workspace. ProjectAppService serializes the two operations' critical
// sections against each other (a mutex, ADR: see the field's doc comment),
// so regardless of which of the two goroutines below the Go scheduler runs
// first, the *final* cached value for proj-1 must deterministically be the
// new workspace ("team-b") — never "default".
func TestRemoveWorkspaceAndAssign_RaceOnCacheSync(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	ws.metas["team-b"] = &orchestrator.WorkspaceMeta{}
	meta := newRecordingMetaStore()
	repo := &stubProjectRepository{
		projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "team-a"},
		},
		existingWorkspaces: map[string]bool{"team-a": true, "team-b": true},
	}
	svc := &ProjectAppService{Projects: repo, Meta: meta, Workspaces: ws}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = svc.RemoveWorkspace("team-a")
	}()
	go func() {
		defer wg.Done()
		_, _ = svc.SetProjectWorkspace("proj-1", "team-b")
	}()
	wg.Wait()

	if got := meta.setWorkspaceIDCalls["proj-1"]; got != "team-b" {
		t.Errorf("final cached workspace_id for proj-1 = %q, want %q (RemoveWorkspace's stale-snapshot cache write must never clobber a concurrent reassignment)", got, "team-b")
	}
}

func TestProjectAppService_RemoveWorkspace_NotFound(t *testing.T) {
	svc := newWorkspaceTestService(newStubWorkspaceStore(), nil, nil)
	err := svc.RemoveWorkspace("ghost")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected 404 StatusError, got %v", err)
	}
}

// TestUpdateWorkspace_ForcePathBlocksMidDelete pins MAJOR 1 (codex review
// round 2, docs/plans/workspace-db-consolidation.md): UpdateWorkspace's
// force=true path does Workspaces.Load (existence check) then, separately,
// Workspaces.Save (an unconditional upsert). Before this fix, a
// RemoveWorkspace call landing in that window would delete the row, and the
// force path's subsequent Save would silently resurrect it via upsert
// semantics — a DELETE the caller believed had taken effect would appear to
// never have happened. This test uses a hook on Load to pause the force path
// while it holds ProjectAppService.mu, starts a concurrent RemoveWorkspace,
// and asserts both that the concurrent Remove is genuinely blocked (not just
// coincidentally slow) and that the workspace ends up removed — never
// resurrected — once both calls finish.
func TestUpdateWorkspace_ForcePathBlocksMidDelete(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}

	loadStarted := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	ws.loadHook = func(slug string) {
		if slug != "team-a" {
			return
		}
		once.Do(func() {
			close(loadStarted)
			<-proceed
		})
	}

	svc := newWorkspaceTestService(ws, map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a"},
	}, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.UpdateWorkspace("team-a", &orchestrator.WorkspaceMeta{HostCommands: []string{"gh"}}, "", true)
	}()

	select {
	case <-loadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateWorkspace's force path never reached Load")
	}

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- svc.RemoveWorkspace("team-a")
	}()

	// Give RemoveWorkspace a chance to run to completion here if s.mu did
	// NOT serialize it against the in-flight force path (the MAJOR 1 bug):
	// without the fix, Remove has nothing to block on and would delete
	// team-a immediately, before UpdateWorkspace's subsequent Save call.
	select {
	case <-removeDone:
		t.Fatal("RemoveWorkspace completed while UpdateWorkspace's force path was still mid-flight (between Load and Save) — s.mu did not serialize them")
	case <-time.After(100 * time.Millisecond):
		// expected: RemoveWorkspace is blocked on s.mu.
	}

	close(proceed) // let the force path's Load return, then Save run.
	wg.Wait()

	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("RemoveWorkspace: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RemoveWorkspace never completed after the force path released s.mu")
	}

	if _, ok := ws.metas["team-a"]; ok {
		t.Error("team-a still exists after a concurrent RemoveWorkspace — the force PUT's upsert resurrected a mid-flight delete (MAJOR 1)")
	}
}

// TestCreateWorkspace_BlocksMidRemove pins MAJOR 1 (codex review round 2):
// CreateWorkspace was widened into the same s.mu critical section as
// UpdateWorkspace/RemoveWorkspace/SetProjectWorkspace for consistency across
// every workspace mutation path. This test starts a CreateWorkspace for a
// slug that does not exist yet, pauses it (via a hook on Create) while it
// holds s.mu, and starts a concurrent RemoveWorkspace for the same slug.
// Without serialization, Remove would run immediately against the
// not-yet-created row and 404; with it, Remove must block until Create
// finishes, then observe (and successfully remove) the freshly created row.
func TestCreateWorkspace_BlocksMidRemove(t *testing.T) {
	ws := newStubWorkspaceStore()

	createStarted := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	ws.createHook = func(slug string) {
		if slug != "team-a" {
			return
		}
		once.Do(func() {
			close(createStarted)
			<-proceed
		})
	}

	svc := newWorkspaceTestService(ws, map[string]*orchestrator.WorkspaceSummary{
		"team-a": {ID: "team-a"},
	}, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.CreateWorkspace("team-a", &orchestrator.WorkspaceMeta{})
	}()

	select {
	case <-createStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("CreateWorkspace never reached Create")
	}

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- svc.RemoveWorkspace("team-a")
	}()

	// Without s.mu covering CreateWorkspace, RemoveWorkspace would run here
	// against a still-nonexistent row and return a 404 immediately.
	select {
	case <-removeDone:
		t.Fatal("RemoveWorkspace completed while CreateWorkspace was still mid-flight — s.mu did not serialize them")
	case <-time.After(100 * time.Millisecond):
		// expected: RemoveWorkspace is blocked on s.mu.
	}

	close(proceed) // let Create finish its insert and release s.mu.
	wg.Wait()

	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("RemoveWorkspace: %v (expected it to observe the freshly created row and succeed)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RemoveWorkspace never completed after CreateWorkspace released s.mu")
	}

	if _, ok := ws.metas["team-a"]; ok {
		t.Error("team-a still exists after RemoveWorkspace succeeded")
	}
}

// ---------------------------------------------------------------------------
// ReloadProjects / CreateProject cache race (MAJOR 1, codex review round 3)
// ---------------------------------------------------------------------------

// TestReloadProjects_BlockedByAssign pins MAJOR 1 (codex review round 3,
// docs/plans/workspace-db-consolidation.md PR4): ReloadProjects snapshots
// every project's workspace_id via ListProjects and then writes it into
// s.Meta's cache via LoadAll — both outside any lock before this fix. A
// concurrent SetProjectWorkspace landing in that window could have its cache
// write clobbered by ReloadProjects's now-stale snapshot arriving after it.
// This test pauses ReloadProjects mid-LoadAll (via a hook), starts a
// concurrent SetProjectWorkspace for the same project targeting a different
// workspace, and asserts that call is genuinely blocked until ReloadProjects
// finishes, and that its cache write survives (is never clobbered).
func TestReloadProjects_BlockedByAssign(t *testing.T) {
	repo := &stubProjectRepository{
		projects:           []*orchestrator.Project{{ID: "proj-1", WorkspaceID: "team-a"}},
		existingWorkspaces: map[string]bool{"team-a": true, "team-b": true},
	}
	meta := newRecordingMetaStore()

	loadAllStarted := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	meta.loadAllHook = func() {
		once.Do(func() {
			close(loadAllStarted)
			<-proceed
		})
	}

	svc := &ProjectAppService{Projects: repo, Meta: meta}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.ReloadProjects()
	}()

	select {
	case <-loadAllStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ReloadProjects never reached LoadAll")
	}

	assignDone := make(chan error, 1)
	go func() {
		_, err := svc.SetProjectWorkspace("proj-1", "team-b")
		assignDone <- err
	}()

	// Without s.mu covering ReloadProjects's ListProjects+LoadAll pair, this
	// concurrent assign would complete here uncontended.
	select {
	case <-assignDone:
		t.Fatal("SetProjectWorkspace completed while ReloadProjects was still mid-flight — s.mu did not serialize them")
	case <-time.After(100 * time.Millisecond):
		// expected: SetProjectWorkspace is blocked on s.mu.
	}

	close(proceed) // let ReloadProjects's LoadAll finish and release s.mu.
	wg.Wait()

	select {
	case err := <-assignDone:
		if err != nil {
			t.Fatalf("SetProjectWorkspace: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetProjectWorkspace never completed after ReloadProjects released s.mu")
	}

	if got := meta.setWorkspaceIDCalls["proj-1"]; got != "team-b" {
		t.Errorf("final cached workspace_id for proj-1 = %q, want %q (ReloadProjects's stale-snapshot cache write must never clobber a concurrent reassignment)", got, "team-b")
	}
}

// TestReloadProjects_BlockedByRemove is the RemoveWorkspace counterpart of
// TestReloadProjects_BlockedByAssign above: a concurrent RemoveWorkspace's
// own cache-sync loop (re-pointing affected projects at the default
// workspace) must not be clobbered by — nor race against — ReloadProjects's
// LoadAll cache write.
func TestReloadProjects_BlockedByRemove(t *testing.T) {
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	repo := &stubProjectRepository{
		projects: []*orchestrator.Project{{ID: "proj-1", WorkspaceID: "team-a"}},
	}
	meta := newRecordingMetaStore()

	loadAllStarted := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	meta.loadAllHook = func() {
		once.Do(func() {
			close(loadAllStarted)
			<-proceed
		})
	}

	svc := &ProjectAppService{Projects: repo, Meta: meta, Workspaces: ws}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.ReloadProjects()
	}()

	select {
	case <-loadAllStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ReloadProjects never reached LoadAll")
	}

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- svc.RemoveWorkspace("team-a")
	}()

	select {
	case <-removeDone:
		t.Fatal("RemoveWorkspace completed while ReloadProjects was still mid-flight — s.mu did not serialize them")
	case <-time.After(100 * time.Millisecond):
		// expected: RemoveWorkspace is blocked on s.mu.
	}

	close(proceed) // let ReloadProjects's LoadAll finish and release s.mu.
	wg.Wait()

	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("RemoveWorkspace: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RemoveWorkspace never completed after ReloadProjects released s.mu")
	}

	if got := meta.setWorkspaceIDCalls["proj-1"]; got != orchestrator.DefaultWorkspaceSlug {
		t.Errorf("final cached workspace_id for proj-1 = %q, want %q (RemoveWorkspace's cache sync must land cleanly after ReloadProjects releases s.mu)", got, orchestrator.DefaultWorkspaceSlug)
	}
}

// createProjectMetaStore is a minimal ProjectAppService.Meta stub for
// TestCreateProject_BlockedByAssign below: Load returns a fixed meta (
// CreateProject dereferences meta.ID immediately, so a nil return — what the
// shared stubProjectMetaStore.Load always returns — would panic), and
// SetWorkspaceID calls are recorded so the test can assert the final cached
// value once both goroutines finish.
type createProjectMetaStore struct {
	meta                *orchestrator.ProjectMeta
	setWorkspaceIDCalls map[string]string
}

func (s *createProjectMetaStore) Load(_ string) (*orchestrator.ProjectMeta, error) {
	return s.meta, nil
}
func (s *createProjectMetaStore) Get(_ string) (*orchestrator.ProjectMeta, bool) { return nil, false }
func (s *createProjectMetaStore) Remove(_ string)                               {}
func (s *createProjectMetaStore) LoadAll(_ []*orchestrator.Project) []error     { return nil }
func (s *createProjectMetaStore) SetWorkspaceID(projectID, workspaceID string) {
	if s.setWorkspaceIDCalls == nil {
		s.setWorkspaceIDCalls = map[string]string{}
	}
	s.setWorkspaceIDCalls[projectID] = workspaceID
}

// TestCreateProject_BlockedByAssign pins MAJOR 1 (codex review round 3,
// docs/plans/workspace-db-consolidation.md PR4): CreateProject's default-
// workspace-assign step (Projects.SetProjectWorkspace + Meta.SetWorkspaceID)
// is itself a workspace-cache writer and must be serialized against a
// concurrent SetProjectWorkspace call the same way every other cache writer
// is — see the mu field's doc comment. This test pauses CreateProject
// mid-assign (via a hook on the repository's SetProjectWorkspace) while it
// holds s.mu, starts a concurrent SetProjectWorkspace for the same project id
// targeting a different workspace, and asserts that call is genuinely
// blocked until CreateProject's own assign step finishes — otherwise the
// concurrent call's cache write could land first and then be silently
// clobbered by CreateProject's own SetWorkspaceID(default) call landing
// after it.
func TestCreateProject_BlockedByAssign(t *testing.T) {
	assignStarted := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once

	repo := &stubProjectRepository{
		existingWorkspaces: map[string]bool{"team-b": true},
	}
	repo.setProjectWorkspaceHook = func(_ string, workspaceID string) {
		if workspaceID != orchestrator.DefaultWorkspaceSlug {
			return // only pause CreateProject's own default-assign call.
		}
		once.Do(func() {
			close(assignStarted)
			<-proceed
		})
	}
	meta := &createProjectMetaStore{meta: &orchestrator.ProjectMeta{ID: "new-proj"}}
	svc := &ProjectAppService{Projects: repo, Meta: meta}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := svc.CreateProject("/some/work/dir"); err != nil {
			t.Errorf("CreateProject: %v", err)
		}
	}()

	select {
	case <-assignStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("CreateProject never reached its default-assign step")
	}

	// CreateProject's own s.Projects.CreateProject call is a no-op stub that
	// does not append to repo.projects, so register the project by hand here
	// — mirrors it having already been inserted by the time a concurrent
	// SetProjectWorkspace call would resolve it via GetProject. Safe to
	// mutate without synchronization: the CreateProject goroutine is parked
	// on <-proceed above and never touches repo.projects itself.
	repo.projects = append(repo.projects, &orchestrator.Project{ID: "new-proj"})

	assignDone := make(chan error, 1)
	go func() {
		_, err := svc.SetProjectWorkspace("new-proj", "team-b")
		assignDone <- err
	}()

	select {
	case <-assignDone:
		t.Fatal("SetProjectWorkspace completed while CreateProject's default-assign was still mid-flight — s.mu did not serialize them")
	case <-time.After(100 * time.Millisecond):
		// expected: SetProjectWorkspace is blocked on s.mu.
	}

	close(proceed) // let CreateProject's default-assign finish and release s.mu.
	wg.Wait()

	select {
	case err := <-assignDone:
		if err != nil {
			t.Fatalf("SetProjectWorkspace: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetProjectWorkspace never completed after CreateProject released s.mu")
	}

	if got := meta.setWorkspaceIDCalls["new-proj"]; got != "team-b" {
		t.Errorf("final cached workspace_id for new-proj = %q, want %q (CreateProject's default-assign cache write must never win a race against a concurrent explicit assign)", got, "team-b")
	}
}

func equalStringSliceForTest(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
