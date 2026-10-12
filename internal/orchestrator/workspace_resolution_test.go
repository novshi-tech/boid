package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectStoreLoadAllIgnoresRepositoryConfiguration(t *testing.T) {
	for _, content := range []string{"id: intruder\nname: Override\nbase_branch: stale\n", "invalid: ["} {
		t.Run(content, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".boid"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".boid", "project.yaml"), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			store := NewProjectStore()
			projects := []*Project{{ID: "registered", WorkDir: dir, UpstreamURL: "https://example.com/team/repository.git"}, {ID: "missing", WorkDir: filepath.Join(dir, "missing.git")}}
			if errs := store.LoadAll(projects); len(errs) != 0 {
				t.Fatalf("LoadAll touched repository: %v", errs)
			}
			got, ok := store.Get("registered")
			if !ok || got.ID != "registered" || got.BaseBranch != "" || len(got.TaskBehaviors) != 0 {
				t.Fatalf("metadata: %+v", got)
			}
			if _, ok := store.Get("intruder"); ok {
				t.Fatal("repository id entered cache")
			}
			if store.Status("missing").State != StatusReady {
				t.Fatal("missing mirror degraded configuration")
			}
		})
	}
}

func TestWorkspaceResolutionIgnoresCachedProjectDefinitions(t *testing.T) {
	ws := NewWorkspaceStore(t.TempDir())
	meta := automationMeta(t)
	meta.BaseBranch = "workspace-main"
	meta.DefaultTaskBehavior = "dev"
	meta.TaskBehaviors = map[string]TaskBehavior{"dev": {Traits: []string{"workspace"}}}
	if err := ws.Save("team", meta); err != nil {
		t.Fatal(err)
	}
	store := NewProjectStore()
	store.SetWorkspaceStore(ws)
	store.Set("p", &ProjectMeta{ID: "p", Name: "Repository", BaseBranch: "stale", DefaultTaskBehavior: "stale", TaskBehaviors: map[string]TaskBehavior{"dev": {Traits: []string{"project"}}, "stale": {}}, Triggers: []Trigger{{Name: "sweep", Run: "echo stale"}}, CardCommands: map[string]CardCommand{"judge": {Label: "Stale", Run: "echo stale"}}})
	store.SetWorkspaceID("p", "team")
	for _, id := range []string{"", "p"} {
		var got *ProjectMeta
		var err error
		if id == "" {
			got, err = store.GetWorkspaceMeta(context.Background(), "team")
		} else {
			got, err = store.GetWithWorkspace(context.Background(), id)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.BaseBranch != "workspace-main" || got.DefaultTaskBehavior != "dev" || got.TaskBehaviors["dev"].Traits[0] != "workspace" {
			t.Fatalf("resolution: %+v", got)
		}
		if _, ok := got.TaskBehaviors["stale"]; ok {
			t.Fatal("stale behavior survived")
		}
		if len(got.Triggers) != 2 || got.Triggers[0].Run != "echo sweep" || got.CardCommands["judge"].Run != "echo custom" {
			t.Fatalf("automation: %+v", got)
		}
	}
}
