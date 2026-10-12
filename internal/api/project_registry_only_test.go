package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestProjectRegistrationWithoutMirror(t *testing.T) {
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team": true}}
	svc := &ProjectAppService{Projects: repo, Meta: orchestrator.NewProjectStore()}
	project, err := svc.CreateProjectFromGitURL(context.Background(), "https://unreachable.test/org/code.git", "team", "registered", "fetch")
	if err != nil {
		t.Fatal(err)
	}
	if project.WorkDir != "" || project.UpstreamURL != "https://unreachable.test/org/code.git" || project.Name != "registered" || project.Perm != "fetch" || project.WorkspaceID != "team" {
		t.Fatalf("registration: %+v", project)
	}
}

func TestDeleteProjectPreservesLegacyMirror(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "objects"), 0700); err != nil {
		t.Fatal(err)
	}
	repo := &stubProjectRepository{projects: []*orchestrator.Project{{ID: "legacy", WorkDir: dir, UpstreamURL: "https://example.test/org/code.git"}}}
	svc := &ProjectAppService{Projects: repo, Meta: orchestrator.NewProjectStore()}
	if err := svc.DeleteProject("legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		t.Fatalf("legacy mirror removed: %v", err)
	}
}

func TestProjectRetiredRoutes(t *testing.T) {
	handler := (&ProjectHandler{Service: &fakeWorkspaceService{}}).Routes()
	for _, path := range []string{"/reload", "/legacy/fetch"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d", path, recorder.Code)
		}
	}
}
