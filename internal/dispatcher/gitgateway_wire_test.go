package dispatcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// --- buildGatewayCloneURL ---

func TestBuildGatewayCloneURL_BuildsFullURL(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", UpstreamURL: "https://github.com/owner/repo.git"},
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1"}

	got := r.buildGatewayCloneURL(spec, "http://10.0.2.2:12345", "job-token-abc")
	want := "http://10.0.2.2:12345/j/job-token-abc/github.com/owner/repo.git"
	if got != want {
		t.Errorf("buildGatewayCloneURL = %q, want %q", got, want)
	}
}

func TestBuildGatewayCloneURL_EmptyWhenGatewayUnwired(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", UpstreamURL: "https://github.com/owner/repo.git"},
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1"}

	if got := r.buildGatewayCloneURL(spec, "", "job-token-abc"); got != "" {
		t.Errorf("buildGatewayCloneURL with empty gatewayURL = %q, want empty", got)
	}
	if got := r.buildGatewayCloneURL(spec, "http://10.0.2.2:1", ""); got != "" {
		t.Errorf("buildGatewayCloneURL with empty gatewayToken = %q, want empty", got)
	}
}

func TestBuildGatewayCloneURL_EmptyWhenUpstreamURLMissing(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1"}, // no UpstreamURL captured
		}},
	}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1"}

	if got := r.buildGatewayCloneURL(spec, "http://10.0.2.2:1", "tok"); got != "" {
		t.Errorf("buildGatewayCloneURL with no upstream_url = %q, want empty", got)
	}
}

func TestBuildGatewayCloneURL_NilProjectsReturnsEmpty(t *testing.T) {
	r := &Runner{}
	spec := &orchestrator.JobSpec{ProjectID: "proj-1"}
	if got := r.buildGatewayCloneURL(spec, "http://10.0.2.2:1", "tok"); got != "" {
		t.Errorf("buildGatewayCloneURL with nil Projects = %q, want empty", got)
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

// --- buildPeerAdvertise (docs/plans/git-gateway-cutover.md PR6 cutover
// 「5. peer advertise の変更」) ---

func TestBuildPeerAdvertise_ResolvesNameCloneURLAndReferencePath(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "peer-1", WorkDir: "/host/peer-1", UpstreamURL: "https://github.com/owner/peer-repo.git"},
		}},
	}
	got := r.buildPeerAdvertise(map[string]string{"peer-1": "/host/peer-1"}, "http://10.0.2.2:12345", "job-token-abc")
	adv, ok := got["peer-1"]
	if !ok {
		t.Fatalf("buildPeerAdvertise = %#v, want an entry for peer-1", got)
	}
	if adv.Name != "peer-repo" {
		t.Errorf("Name = %q, want peer-repo", adv.Name)
	}
	if want := "http://10.0.2.2:12345/j/job-token-abc/github.com/owner/peer-repo.git"; adv.CloneURL != want {
		t.Errorf("CloneURL = %q, want %q", adv.CloneURL, want)
	}
	if want := "/mnt/refs/peers/peer-1.git"; adv.ReferencePath != want {
		t.Errorf("ReferencePath = %q, want %q", adv.ReferencePath, want)
	}
	// CloneDir (workspace 親化リファクタリング, nose 2026-07-13 decision):
	// fakeProjectLookup never populates Meta (mirroring the real
	// DBProjectCatalog gap documented on buildPeerAdvertise's CloneDir
	// assignment), so this degrades to filepath.Base(WorkDir).
	if want := "/workspace/peer-1"; adv.CloneDir != want {
		t.Errorf("CloneDir = %q, want %q", adv.CloneDir, want)
	}
}

func TestBuildPeerAdvertise_SkipsPeerWithNoUpstreamURL(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "peer-1"}, // no UpstreamURL captured
		}},
	}
	got := r.buildPeerAdvertise(map[string]string{"peer-1": "/host/peer-1"}, "http://10.0.2.2:1", "tok")
	if got != nil {
		t.Fatalf("buildPeerAdvertise = %#v, want nil when the only peer has no upstream_url", got)
	}
}

func TestBuildPeerAdvertise_NilWhenGatewayUnwiredOrNoProjects(t *testing.T) {
	peers := map[string]string{"peer-1": "/host/peer-1"}
	r := &Runner{Projects: fakeProjectLookup{projects: []*orchestrator.Project{
		{ID: "peer-1", UpstreamURL: "https://github.com/owner/peer-repo.git"},
	}}}
	if got := r.buildPeerAdvertise(peers, "", "tok"); got != nil {
		t.Errorf("buildPeerAdvertise with empty gatewayURL = %#v, want nil", got)
	}
	if got := r.buildPeerAdvertise(peers, "http://10.0.2.2:1", ""); got != nil {
		t.Errorf("buildPeerAdvertise with empty gatewayToken = %#v, want nil", got)
	}
	if got := (&Runner{}).buildPeerAdvertise(peers, "http://10.0.2.2:1", "tok"); got != nil {
		t.Errorf("buildPeerAdvertise with nil Projects = %#v, want nil", got)
	}
	if got := r.buildPeerAdvertise(nil, "http://10.0.2.2:1", "tok"); got != nil {
		t.Errorf("buildPeerAdvertise with no peers = %#v, want nil", got)
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

// gwFakeRuntimeStartError is a JobRuntime stub whose Start always fails, so
// tests can exercise Dispatch's post-token-registration error path (the
// docs/plans/git-gateway-cutover.md PR5 token-leak fix guard below): by the
// time launchSandbox calls Runtime.Start, both the broker token and the git
// gateway job token have already been registered.
type gwFakeRuntimeStartError struct{}

func (r *gwFakeRuntimeStartError) Start(_ context.Context, _ RuntimeStartSpec) (*RuntimeHandle, error) {
	return nil, fmt.Errorf("boom: runtime start failed")
}
func (r *gwFakeRuntimeStartError) Attach(_ context.Context, _ string, _ RuntimeAttachRequest) error {
	return ErrRuntimeUnsupported
}
func (r *gwFakeRuntimeStartError) Resize(_ context.Context, _ string, _ TerminalSize) error {
	return ErrRuntimeUnsupported
}
func (r *gwFakeRuntimeStartError) Wait(_ context.Context, _ string) (RuntimeExit, error) {
	return RuntimeExit{}, ErrRuntimeUnsupported
}
func (r *gwFakeRuntimeStartError) Stop(_ context.Context, _ string) error { return nil }
func (r *gwFakeRuntimeStartError) Signal(_ context.Context, _ string, _ syscall.Signal) error {
	return nil
}

// gwFakeFailingBroker is a minimal CommandBroker stub that hands out a fresh
// token on every RegisterCommands call and records both registrations and
// unregistrations, so a test can assert every token that went out also came
// back.
type gwFakeFailingBroker struct {
	registered   []string
	unregistered []string
}

func (b *gwFakeFailingBroker) RegisterCommands(_ map[string]orchestrator.CommandDef, _ map[string]sandbox.BuiltinPolicy, _ sandbox.TokenContext, _ SecretResolver) string {
	token := fmt.Sprintf("broker-tok-%d", len(b.registered)+1)
	b.registered = append(b.registered, token)
	return token
}
func (b *gwFakeFailingBroker) UnregisterCommandToken(token string) {
	b.unregistered = append(b.unregistered, token)
}
func (b *gwFakeFailingBroker) SocketPath() string { return "/tmp/fake-broker.sock" }

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

// TestDispatch_RegistersGatewayTokenWithSecretNamespace is the Dispatch-level
// guard for post-cutover 改善 §1 (workspace-scoped PAT namespace): it proves
// registerGatewayToken passes spec.SecretNamespace through to
// gitgateway.Registry.Register (internal/dispatcher/gitgateway_wire.go's
// r.GitGateway.Register(repos, spec.SecretNamespace) call), so the real
// Registry entry created by a live Dispatch carries the namespace that
// Server.ServeHTTP will later read back out via Lookup to scope credential
// resolution. spec.SecretNamespace itself is populated upstream of Dispatch
// by orchestrator.ProjectStore.GetWithWorkspace (already-landed wiring —
// this test only pins the one remaining hop: JobSpec field → Registry
// entry).
func TestDispatch_RegistersGatewayTokenWithSecretNamespace(t *testing.T) {
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
		ProjectID:       "proj-1",
		Argv:            []string{"echo", "hi"},
		Kind:            orchestrator.JobKindHook,
		Visibility:      orchestrator.Visibility{Writable: true},
		SecretNamespace: "ws-scoped-pat",
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	token, ok := r.gatewayTokens[jobID]
	if !ok || token == "" {
		t.Fatalf("gatewayTokens[%q] not registered", jobID)
	}

	entry, valid := registry.Lookup(token)
	if !valid {
		t.Fatal("registry.Lookup: token registered by Dispatch was not found in the real Registry")
	}
	if entry.Namespace != "ws-scoped-pat" {
		t.Errorf("entry.Namespace = %q, want %q (spec.SecretNamespace should have been threaded through Register)", entry.Namespace, "ws-scoped-pat")
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

// TestDispatch_RuntimeStartError_UnregistersBrokerAndGatewayTokens is the
// regression guard for the PR4-review token-leak fix
// (docs/plans/git-gateway-cutover.md PR5 scope, "Dispatch エラーパスの token
// leak"): before this fix, a Runtime.Start failure — which happens well
// after both the broker token (RegisterCommands) and the git gateway job
// token (registerGatewayToken) are registered — left both tokens tracked in
// r.jobTokens / r.gatewayTokens forever, since only a successful launch ever
// scheduled UnregisterJob. Dispatch's defer now calls UnregisterJob
// unconditionally on any non-nil return error, so both tokens must come
// back out here.
func TestDispatch_RuntimeStartError_UnregistersBrokerAndGatewayTokens(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/tmp", UpstreamURL: "https://github.com/owner/repo.git",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gwURL := "http://10.0.2.2:9"
	registry := gitgateway.NewRegistry()
	broker := &gwFakeFailingBroker{}
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Sandbox:    &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:    &gwFakeRuntimeStartError{},
		Broker:     broker,
		BoidBinary: "/boid",
		GitGateway: registry,
		GatewayURL: &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{Writable: true},
		// A resolvable host command (path exists) so Dispatch's broker
		// registration branch actually fires and leaves a broker token to
		// leak, alongside the git gateway token registerGatewayToken always
		// registers when GitGateway is wired.
		HostCommands: map[string]orchestrator.CommandDef{
			"echo-tool": {Path: "/bin/echo"},
		},
	}

	_, err := r.Dispatch(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("expected Dispatch to return an error when Runtime.Start fails")
	}

	if len(broker.registered) == 0 {
		t.Fatal("test setup invalid: broker token was never registered, so there is nothing to prove got unregistered")
	}
	if len(broker.unregistered) != len(broker.registered) {
		t.Errorf("broker.unregistered = %v, want every registered token (%v) unregistered on Dispatch failure",
			broker.unregistered, broker.registered)
	}
	if len(r.gatewayTokens) != 0 {
		t.Errorf("gatewayTokens leaked after Dispatch failure: %#v", r.gatewayTokens)
	}
	if len(r.jobTokens) != 0 {
		t.Errorf("jobTokens (broker) leaked after Dispatch failure: %#v", r.jobTokens)
	}
}

// --- Dispatch clone-mode upstream_url guard (Opus review #4) ---

// erroringProjectLookup is a ProjectLookup that fails GetProject with a
// canned error, simulating a torn Projects registry / DB read failure mid-
// dispatch. Used only in the clone-mode fail-loud tests below.
type erroringProjectLookup struct {
	err error
}

func (e erroringProjectLookup) GetProject(_ string) (*orchestrator.Project, error) {
	return nil, e.err
}

func (e erroringProjectLookup) ListProjects() ([]*orchestrator.Project, error) {
	return nil, nil
}

// TestDispatch_CloneMode_ProjectLookupError_FailsLoud pins the fail-loud
// contract for a torn Projects registry: when a Visibility.Clone-declaring
// spec dispatches and GetProject returns an error, Dispatch must surface a
// clear message (naming the project ID) rather than silently skipping the
// upstream_url check and continuing on to a runtime "git clone" failure
// deep in the sandbox (docs/plans/git-gateway-cutover.md PR6 cutover, Opus
// review #4).
func TestDispatch_CloneMode_ProjectLookupError_FailsLoud(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/tmp", UpstreamURL: "https://github.com/owner/repo.git",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	r := &Runner{
		DB:         d.Conn,
		Projects:   erroringProjectLookup{err: fmt.Errorf("db read failed")},
		Sandbox:    &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:    &gwFakeRuntime{},
		BoidBinary: "/boid",
	}
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			Clone: &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	_, err := r.Dispatch(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("Dispatch: expected error when Projects.GetProject fails for a clone-mode job")
	}
	if !strings.Contains(err.Error(), "look up project") {
		t.Errorf("error = %q, want to mention \"look up project\"", err.Error())
	}
	if !strings.Contains(err.Error(), "proj-1") {
		t.Errorf("error = %q, want to name the project id proj-1", err.Error())
	}
}

// TestDispatch_CloneMode_ProjectNotFound_FailsLoud pins the same contract
// for a nil-project-row case (registry drift: the caller has a project ID
// that no longer resolves).
func TestDispatch_CloneMode_ProjectNotFound_FailsLoud(t *testing.T) {
	d := newGatewayTestDB(t)
	r := &Runner{
		DB:         d.Conn,
		Projects:   fakeProjectLookup{projects: nil}, // GetProject returns (nil, nil)
		Sandbox:    &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:    &gwFakeRuntime{},
		BoidBinary: "/boid",
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project (DB row for CreateJob FK): %v", err)
	}
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			Clone: &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	_, err := r.Dispatch(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("Dispatch: expected error when Projects.GetProject returns (nil, nil) for a clone-mode job")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want to mention \"not found\"", err.Error())
	}
}

// TestDispatch_CloneMode_MissingUpstreamURL_FailsLoud pins the case where
// the project row exists but has no upstream_url captured yet (pre-PR2
// project that skipped backfill). RequireUpstreamURL fires and surfaces a
// clear message.
func TestDispatch_CloneMode_MissingUpstreamURL_FailsLoud(t *testing.T) {
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
	}
	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{
			Clone: &orchestrator.CloneDeclaration{Branch: "main", BaseBranch: "main", CheckoutOnly: true},
		},
	}
	_, err := r.Dispatch(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("Dispatch: expected error when the project has no captured upstream_url")
	}
	if !strings.Contains(err.Error(), "upstream_url") {
		t.Errorf("error = %q, want to mention \"upstream_url\"", err.Error())
	}
}
