package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestProjectRegistrationIgnoresCommittedConfiguration(t *testing.T) {
	for _, content := range []string{"id: intruder\nname: Override\ntask_behaviors:\n  stale: {}\n", "invalid: ["} {
		t.Run(content, func(t *testing.T) {
			src := setupGitURLSourceRepo(t, "initial", "Initial")
			if err := os.WriteFile(filepath.Join(src, ".boid", "project.yaml"), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			runGitCmd(t, src, "add", ".")
			runGitCmd(t, src, "commit", "-q", "-m", "configuration")
			repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team": true}}
			svc, _ := newGitURLTestService(t, repo)
			project, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team", "registered-name", "")
			if err != nil {
				t.Fatal(err)
			}
			id, err := deriveProjectIDFromURL(fileURL(src))
			if err != nil {
				t.Fatal(err)
			}
			if project.ID != id || project.Name != "registered-name" || project.Meta.Name != "registered-name" || len(project.Meta.TaskBehaviors) != 0 {
				t.Fatalf("registration: %+v", project)
			}
			repo.projects = []*orchestrator.Project{project}
			if err := os.WriteFile(filepath.Join(src, ".boid", "project.yaml"), []byte("id: drift\nname: Changed\n"), 0644); err != nil {
				t.Fatal(err)
			}
			runGitCmd(t, src, "add", ".")
			runGitCmd(t, src, "commit", "-q", "-m", "drift")
			fetched, err := svc.FetchProject(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fetched.ID != id || fetched.Meta.Name != "registered-name" || len(fetched.Meta.TaskBehaviors) != 0 {
				t.Fatalf("fetch: %+v", fetched)
			}
			if _, ok := svc.Meta.Get("drift"); ok {
				t.Fatal("repository id entered cache")
			}
		})
	}
}
