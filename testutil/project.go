package testutil

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

// SeedProjectFixture places a test identity in the registry and its runtime definitions in the workspace.
func SeedProjectFixture(t *testing.T, ts *TestServer, dir string, output any) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".boid", "project.yaml"))
	if err != nil {
		return err
	}
	var meta orchestrator.ProjectMeta
	if err = yaml.Unmarshal(data, &meta); err != nil {
		return err
	}
	origin, err := exec.Command("git", "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return err
	}
	url, err := dispatcher.NormalizeOriginURL(string(origin))
	if err != nil {
		return err
	}
	project := &orchestrator.Project{ID: meta.ID, WorkDir: dir, UpstreamURL: url, Perm: "push"}
	if err = orchestrator.CreateProject(ts.Server.DB(), project); err != nil {
		return err
	}
	project.WorkspaceID = orchestrator.DefaultWorkspaceSlug
	if err = orchestrator.SetProjectWorkspace(ts.Server.DB(), project.ID, project.WorkspaceID); err != nil {
		return err
	}
	repo := orchestrator.NewWorkspaceRepository(ts.Server.DB())
	ws, err := repo.Load(project.WorkspaceID)
	if err != nil {
		return err
	}
	if ws.TaskBehaviors == nil {
		ws.TaskBehaviors = make(map[string]orchestrator.TaskBehavior)
	}
	for name, behavior := range meta.TaskBehaviors {
		ws.TaskBehaviors[name] = behavior
	}
	if meta.DefaultTaskBehavior != "" {
		ws.DefaultTaskBehavior = meta.DefaultTaskBehavior
	}
	if meta.BaseBranch != "" {
		ws.BaseBranch = meta.BaseBranch
	}
	if len(meta.Env) > 0 {
		ws.Env = meta.Env
	}
	if len(meta.Triggers) > 0 {
		ws.Triggers = meta.Triggers
	}
	if len(meta.Signals.Sources) > 0 {
		ws.Signals = meta.Signals
	}
	if len(meta.CardCommands) > 0 {
		ws.CardCommands = meta.CardCommands
		ws.CardEvents = meta.CardEvents
	}
	if err = repo.Save(project.WorkspaceID, ws); err != nil {
		return err
	}
	store := ts.Server.Store()
	store.SetSynthesizedMeta(project.ID, &orchestrator.ProjectMeta{ID: project.ID, Name: meta.Name, NameSource: "explicit"})
	store.SetWorkspaceID(project.ID, project.WorkspaceID)
	hydrated, err := store.GetWithWorkspace(context.Background(), project.ID)
	if err != nil {
		return err
	}
	project.Meta = *hydrated
	if output != nil {
		data, err = json.Marshal(project)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, output)
	}
	return nil
}

// ServeGitRepository exposes a local fixture through Git's dumb HTTP protocol.
func ServeGitRepository(t *testing.T, dir string) string {
	t.Helper()
	if out, err := exec.Command("git", "-C", dir, "update-server-info").CombinedOutput(); err != nil {
		t.Fatalf("update-server-info: %v: %s", err, out)
	}
	server := httptest.NewTLSServer(http.StripPrefix("/fixtures/repository.git/", http.FileServer(http.Dir(filepath.Join(dir, ".git")))))
	t.Cleanup(server.Close)
	certPath := filepath.Join(t.TempDir(), "git-ca.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", certPath)
	return server.URL + "/fixtures/repository.git"
}
