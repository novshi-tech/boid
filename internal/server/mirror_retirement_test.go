package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestBuildProjectStoreLegacyMirrorsAreMetadataOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bin := t.TempDir()
	marker := filepath.Join(bin, "git-called")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	d := openTestDB(t)
	repository := orchestrator.NewProjectRepository(d.Conn)
	for _, project := range []*orchestrator.Project{{ID: "legacy", Name: "Registered", WorkDir: "/missing/mirror.git", UpstreamURL: "https://github.com/org/repo.git"}, {ID: "no-url", Name: "NeedsRegistration", WorkDir: "/missing/other.git"}} {
		if err := repository.CreateProject(project); err != nil {
			t.Fatal(err)
		}
	}
	store, _, err := buildProjectStore(Config{DBPath: ":memory:"}, d.Conn, repository)
	if err != nil {
		t.Fatal(err)
	}
	if meta, ok := store.Get("legacy"); !ok || meta.Name != "Registered" {
		t.Fatalf("legacy identity: %+v %v", meta, ok)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("startup invoked git: %v", err)
	}
}
