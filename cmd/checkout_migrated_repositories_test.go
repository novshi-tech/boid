package cmd

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestMigratedFetchRepositoriesResolveByWorkspaceName(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`DELETE FROM schema_migrations WHERE version='0063_register_extra_repositories'; INSERT INTO workspaces(slug,extra_repos) VALUES ('owner','[]'),('team','["git@github.com:org/lib.git"]'); INSERT INTO projects(id,name,work_dir,upstream_url,perm) VALUES ('existing','lib','','https://github.com/org/lib.git','push'); INSERT INTO project_workspaces(project_id,workspace_id) VALUES ('existing','owner')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	repository := orchestrator.NewProjectRepository(d.Conn)
	store := orchestrator.NewProjectStore()
	rows, err := repository.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if errs := store.LoadAll(rows); len(errs) != 0 {
		t.Fatal(errs)
	}
	service := &api.ProjectAppService{Projects: repository, Meta: store}
	for _, workspace := range []string{"owner", "team"} {
		projects, err := service.ListProjects(workspace)
		if err != nil || len(projects) != 1 {
			t.Fatalf("%s: projects=%v err=%v", workspace, projects, err)
		}
		project := projects[0]
		matches, err := service.ResolveProjectRef(project.Name)
		if err != nil || len(matches) != 1 || matches[0].WorkspaceID != workspace {
			t.Fatalf("%s ref: %v %v", workspace, matches, err)
		}
		data, err := json.Marshal(projects)
		if err != nil {
			t.Fatal(err)
		}
		repo, err := checkoutNamedRepo(project.Name, data)
		if err != nil || repo != "github.com/org/lib" {
			t.Fatalf("%s checkout: %s %v", workspace, repo, err)
		}
		if workspace == "team" && project.Perm != "fetch" {
			t.Fatal("migration lost fetch permission")
		}
	}
}
