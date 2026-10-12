package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

func TestWorkspaceEnvelopeRejectsRetiredExtraRepos(t *testing.T) {
	_, err := DecodeWorkspaceEnvelopeDocuments([]byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team\nspec:\n  extra_repos: [https://github.com/org/lib.git]\n"))
	if err == nil || !strings.Contains(err.Error(), "extra_repos has retired") {
		t.Fatalf("error=%v", err)
	}
}

func TestLegacyWorkspaceExtraReposBecomeFetchRegistrations(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("extra_repos:\n  - git@github.com:org/lib.git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	if err := MigrateWorkspaceYAMLToDB(d.Conn, dir, t.TempDir(), NewProjectRepository(d.Conn)); err != nil {
		t.Fatal(err)
	}
	projects, err := ListProjects(d.Conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Perm != "fetch" || projects[0].WorkspaceID != "team" || projects[0].WorkDir != "" {
		t.Fatalf("registrations: %+v", projects)
	}
}
