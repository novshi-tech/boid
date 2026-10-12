package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestLegacyMirrorRowsDispatchWithoutRepositoryAccess(t *testing.T) {
	d := newGatewayTestDB(t)
	mirror := filepath.Join(t.TempDir(), "missing-mirror.git")
	projects := orchestrator.NewProjectRepository(d.Conn)
	if err := projects.CreateProject(&orchestrator.Project{ID: "legacy", Name: "Legacy", WorkDir: mirror, UpstreamURL: "https://github.com/org/primary.git", Perm: "push"}); err != nil {
		t.Fatal(err)
	}
	if err := projects.AssignWorkspaceIfExists("legacy", orchestrator.DefaultWorkspaceSlug); err != nil {
		t.Fatal(err)
	}
	if err := projects.CreateProject(&orchestrator.Project{ID: "fetch", Name: "Library", WorkDir: "", UpstreamURL: "https://github.com/org/library.git", Perm: "fetch"}); err != nil {
		t.Fatal(err)
	}
	if err := projects.AssignWorkspaceIfExists("fetch", orchestrator.DefaultWorkspaceSlug); err != nil {
		t.Fatal(err)
	}
	store := orchestrator.NewProjectStore()
	rows, err := projects.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if errs := store.LoadAll(rows); len(errs) > 0 {
		t.Fatal(errs)
	}
	gatewayURL := "http://gateway.invalid"
	registry := gitgateway.NewRegistry()
	broker := &recordingBroker{}
	runner := &Runner{Broker: broker, DB: d.Conn, Projects: orchestrator.DBProjectCatalog{DB: d.Conn}, Backend: &gwFakeBackend{}, BoidBinary: "/boid", GitGateway: registry, GatewayURL: &gatewayURL}
	spec := &orchestrator.JobSpec{ProjectID: "legacy", Kind: orchestrator.JobKindHook, Argv: []string{"echo", "ok"}, Visibility: orchestrator.Visibility{Writable: true, Checkout: true}, HostCommands: map[string]orchestrator.CommandDef{"env": {Path: "/usr/bin/env", Env: map[string]string{"GH_REPO": "${boid:repo_slug}"}}}}
	job, err := runner.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.UnregisterJob(job)
	if got := broker.lastCommands["env"].Env["GH_REPO"]; got != "github.com/org/primary" {
		t.Fatalf("GH_REPO=%q", got)
	}
	entry, ok := registry.Lookup(runner.gatewayTokens[job])
	if !ok {
		t.Fatal("missing gateway token")
	}
	if entry.Repos[gitgateway.NewRepoKey("github.com", "org", "primary")] != gitgateway.PermFetchPush {
		t.Fatal("legacy primary lost push permission")
	}
	if entry.Repos[gitgateway.NewRepoKey("github.com", "org", "library")] != gitgateway.PermFetch {
		t.Fatal("library lost fetch permission")
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatalf("mirror accessed or created: %v", err)
	}
}

func TestMigratedExtraRepositoryGatewayFetchPermission(t *testing.T) {
	d := newGatewayTestDB(t)
	if _, err := d.Conn.Exec(`DELETE FROM schema_migrations WHERE version='0063_register_extra_repositories'; INSERT INTO workspaces(slug,extra_repos) VALUES ('default','["https://github.com/org/lib.git"]') ON CONFLICT(slug) DO UPDATE SET extra_repos=excluded.extra_repos`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Projects: orchestrator.DBProjectCatalog{DB: d.Conn}}
	repos := runner.buildGatewayRepos(&orchestrator.JobSpec{Visibility: orchestrator.Visibility{Writable: true}}, "default")
	if len(repos) != 1 || repos[gitgateway.NewRepoKey("github.com", "org", "lib")] != gitgateway.PermFetch {
		t.Fatalf("migrated gateway permissions: %v", repos)
	}
}
