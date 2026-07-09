package dispatcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// newGatewayTestDB returns an in-memory, migrated DB. Deliberately not
// testutil.NewTestDB: testutil transitively imports internal/server (which
// imports internal/dispatcher), so importing testutil from an
// internal-package (package dispatcher, not dispatcher_test) test file would
// be an import cycle. internal/db and internal/db/migrate have no such
// dependency (see scripts/check-internal-architecture.sh: "internal/db
// should not depend on other internal packages"), so opening/migrating
// directly here — mirroring worktree_resolver_test.go's existing pattern in
// this same package — sidesteps the cycle.
func newGatewayTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// --- repoKeyFromUpstreamURL ---

func TestRepoKeyFromUpstreamURL_HTTPS(t *testing.T) {
	key, err := repoKeyFromUpstreamURL("https://github.com/owner/repo.git")
	if err != nil {
		t.Fatalf("repoKeyFromUpstreamURL: %v", err)
	}
	if want := gitgateway.NewRepoKey("github.com", "owner", "repo"); key != want {
		t.Errorf("key = %q, want %q", key, want)
	}
}

func TestRepoKeyFromUpstreamURL_SSH(t *testing.T) {
	key, err := repoKeyFromUpstreamURL("git@bitbucket.org:owner/repo.git")
	if err != nil {
		t.Fatalf("repoKeyFromUpstreamURL: %v", err)
	}
	if want := gitgateway.NewRepoKey("bitbucket.org", "owner", "repo"); key != want {
		t.Errorf("key = %q, want %q", key, want)
	}
}

func TestRepoKeyFromUpstreamURL_MalformedReturnsError(t *testing.T) {
	if _, err := repoKeyFromUpstreamURL("not-a-url"); err == nil {
		t.Fatal("expected error for malformed upstream_url, got nil")
	}
}

func TestRepoKeyFromUpstreamURL_TooManyPathSegmentsReturnsError(t *testing.T) {
	// GitLab-style nested subgroup: host/group/subgroup/repo has 4 segments,
	// which the gateway's fixed host/owner/repo route pattern cannot express
	// (docs/plans/git-gateway-cutover.md: GitHub/Bitbucket Cloud only).
	if _, err := repoKeyFromUpstreamURL("https://gitlab.example.com/group/subgroup/repo.git"); err == nil {
		t.Fatal("expected error for a 4-segment slug, got nil")
	}
}

// --- buildGatewayRepos ---

func TestBuildGatewayRepos_SelfProjectWritableGetsFetchPush(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1", UpstreamURL: "https://github.com/owner/repo.git"},
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1", Visibility: orchestrator.Visibility{Writable: true}}

	repos := r.buildGatewayRepos(spec, "ws-1")
	key := gitgateway.NewRepoKey("github.com", "owner", "repo")
	if perm, ok := repos[key]; !ok || perm != gitgateway.PermFetchPush {
		t.Fatalf("repos[%q] = %v, %v; want PermFetchPush, true", key, perm, ok)
	}
}

func TestBuildGatewayRepos_SelfProjectReadonlyGetsFetchOnly(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1", UpstreamURL: "https://github.com/owner/repo.git"},
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1", Visibility: orchestrator.Visibility{Writable: false}}

	repos := r.buildGatewayRepos(spec, "ws-1")
	key := gitgateway.NewRepoKey("github.com", "owner", "repo")
	if perm, ok := repos[key]; !ok || perm != gitgateway.PermFetch {
		t.Fatalf("repos[%q] = %v, %v; want PermFetch, true", key, perm, ok)
	}
}

func TestBuildGatewayRepos_WorkspacePeerIsFetchOnly(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1", UpstreamURL: "https://github.com/owner/repo.git"},
			{ID: "proj-2", WorkspaceID: "ws-1", UpstreamURL: "https://github.com/owner/peer.git"},
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1", Visibility: orchestrator.Visibility{Writable: true}}

	repos := r.buildGatewayRepos(spec, "ws-1")
	selfKey := gitgateway.NewRepoKey("github.com", "owner", "repo")
	peerKey := gitgateway.NewRepoKey("github.com", "owner", "peer")
	if perm := repos[selfKey]; perm != gitgateway.PermFetchPush {
		t.Errorf("self perm = %v, want PermFetchPush", perm)
	}
	if perm, ok := repos[peerKey]; !ok || perm != gitgateway.PermFetch {
		t.Fatalf("peer perm = %v, %v; want PermFetch, true (peers are always fetch-only)", perm, ok)
	}
}

func TestBuildGatewayRepos_OtherWorkspaceProjectExcluded(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1", UpstreamURL: "https://github.com/owner/repo.git"},
			{ID: "proj-3", WorkspaceID: "ws-2", UpstreamURL: "https://github.com/owner/other-ws.git"},
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1"}

	repos := r.buildGatewayRepos(spec, "ws-1")
	otherKey := gitgateway.NewRepoKey("github.com", "owner", "other-ws")
	if _, ok := repos[otherKey]; ok {
		t.Fatalf("repos should not contain a project from a different workspace: %#v", repos)
	}
}

func TestBuildGatewayRepos_WorkspaceExtraReposIsFetchOnly(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1", UpstreamURL: "https://github.com/owner/repo.git"},
		}},
		Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
			"ws-1": {ExtraRepos: []string{"https://github.com/other-org/private-lib.git"}},
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1"}

	repos := r.buildGatewayRepos(spec, "ws-1")
	extraKey := gitgateway.NewRepoKey("github.com", "other-org", "private-lib")
	if perm, ok := repos[extraKey]; !ok || perm != gitgateway.PermFetch {
		t.Fatalf("extra_repos perm = %v, %v; want PermFetch, true", perm, ok)
	}
}

func TestBuildGatewayRepos_MissingUpstreamURLSkipsSelfProjectSilently(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1"}, // no UpstreamURL captured yet
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1"}

	repos := r.buildGatewayRepos(spec, "ws-1")
	if len(repos) != 0 {
		t.Fatalf("repos = %#v, want empty (no upstream_url to register)", repos)
	}
}

func TestBuildGatewayRepos_NilProjectsReturnsNil(t *testing.T) {
	r := &Runner{}
	if repos := r.buildGatewayRepos(&orchestrator.JobSpec{ProjectID: "proj-1"}, "ws-1"); repos != nil {
		t.Fatalf("repos = %#v, want nil when Projects is unwired", repos)
	}
}

// --- Dispatch-level gateway lifecycle wiring ---

// gwFakeSandboxPrep is a minimal SandboxPreparer stub, mirroring
// dispatcher_test's fakeSandboxPrep (which lives in a different Go package —
// package dispatcher_test — and so cannot be reused directly from this
// internal-package test file).
type gwFakeSandboxPrep struct{ dir string }

func (p *gwFakeSandboxPrep) PrepareSandbox(_ sandbox.Spec) (*PreparedSandbox, error) {
	specPath := filepath.Join(p.dir, "runner-spec.json")
	if err := os.WriteFile(specPath, []byte("{}"), 0o600); err != nil {
		return nil, fmt.Errorf("write runner spec: %w", err)
	}
	return &PreparedSandbox{
		SpecPath:  specPath,
		StatePath: filepath.Join(p.dir, "runner-state.json"),
	}, nil
}

// gwFakeRuntime is a minimal JobRuntime stub that always starts successfully
// and never completes on its own (Wait blocks forever in production; here it
// just reports ErrRuntimeUnsupported since the test never calls Wait).
type gwFakeRuntime struct{ nextID int }

func (r *gwFakeRuntime) Start(_ context.Context, _ RuntimeStartSpec) (*RuntimeHandle, error) {
	r.nextID++
	return &RuntimeHandle{ID: fmt.Sprintf("gw-runtime-%d", r.nextID)}, nil
}
func (r *gwFakeRuntime) Attach(_ context.Context, _ string, _ RuntimeAttachRequest) error {
	return ErrRuntimeUnsupported
}
func (r *gwFakeRuntime) Resize(_ context.Context, _ string, _ TerminalSize) error {
	return ErrRuntimeUnsupported
}
func (r *gwFakeRuntime) Wait(_ context.Context, _ string) (RuntimeExit, error) {
	return RuntimeExit{}, ErrRuntimeUnsupported
}
func (r *gwFakeRuntime) Stop(_ context.Context, _ string) error { return nil }
func (r *gwFakeRuntime) Signal(_ context.Context, _ string, _ syscall.Signal) error {
	return nil
}

// TestDispatch_RegistersAndUnregistersGatewayToken is the Dispatch-level
// guard for the gateway wiring seam: it proves Dispatch actually reaches
// registerGatewayToken (not just that the helper works in isolation — see
// .claude/skills/boid-review's "wiring seam" doctrine on why testing only
// the inner helper would miss a dropped call site) and that UnregisterJob
// revokes the token in the real gitgateway.Registry.
func TestDispatch_RegistersAndUnregistersGatewayToken(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/tmp", UpstreamURL: "https://github.com/owner/repo.git",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gwURL := "http://10.0.2.2:9"
	registry := gitgateway.NewRegistry()
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Sandbox:    &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:    &gwFakeRuntime{},
		BoidBinary: "/boid",
		GitGateway: registry,
		GatewayURL: &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{Writable: true},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	token, ok := r.gatewayTokens[jobID]
	if !ok || token == "" {
		t.Fatalf("gatewayTokens[%q] not registered (rtInfo.GatewayJobToken wiring likely dropped)", jobID)
	}

	entry, valid := registry.Lookup(token)
	if !valid {
		t.Fatal("registry.Lookup: token registered by Dispatch was not found in the real Registry")
	}
	repoKey := gitgateway.NewRepoKey("github.com", "owner", "repo")
	if perm, ok := entry.Repos[repoKey]; !ok || perm != gitgateway.PermFetchPush {
		t.Errorf("registered entry.Repos[%q] = %v, %v; want PermFetchPush, true", repoKey, perm, ok)
	}

	r.UnregisterJob(jobID)

	if _, stillValid := registry.Lookup(token); stillValid {
		t.Fatal("token should be revoked from the Registry after UnregisterJob")
	}
	if _, tracked := r.gatewayTokens[jobID]; tracked {
		t.Fatal("gatewayTokens entry should be removed after UnregisterJob")
	}
}

// TestDispatch_GatewayUnwired_NoTokenNoPanic verifies the nil-GitGateway path
// (test wiring / a daemon build without the gateway constructed) leaves
// SandboxRuntimeInfo's gateway fields empty and Dispatch/UnregisterJob do not
// panic.
func TestDispatch_GatewayUnwired_NoTokenNoPanic(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Sandbox:    &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:    &gwFakeRuntime{},
		BoidBinary: "/boid",
		// GitGateway deliberately left nil.
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1", Argv: []string{"echo", "hi"}, Kind: orchestrator.JobKindHook}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, ok := r.gatewayTokens[jobID]; ok {
		t.Fatal("gatewayTokens should stay empty when GitGateway is unwired")
	}
	r.UnregisterJob(jobID) // must not panic
}
