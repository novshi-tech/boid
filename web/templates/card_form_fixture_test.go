package templates

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestWriteCardFormFixture(t *testing.T) {
	dir := os.Getenv("BOID_UI_FIXTURE_DIR")
	if dir == "" {
		t.Skip("BOID_UI_FIXTURE_DIR is not set")
	}
	var projects []*orchestrator.Project
	for _, ws := range []string{"default", "team"} {
		meta := orchestrator.DefaultMetaprojectMeta(ws)
		projects = append(projects, &orchestrator.Project{ID: meta.ID, WorkspaceID: ws, Meta: *meta})
	}
	projects = append(projects, &orchestrator.Project{ID: "custom", WorkspaceID: "team", Meta: orchestrator.ProjectMeta{Name: "Custom judge"}})
	var page bytes.Buffer
	if err := TaskNew(projects, "", nil).Render(context.Background(), &page); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		t.Fatal(err)
	}
	css, err := os.ReadFile("../static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "card-new.html"), page.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "static", "style.css"), css, 0o644); err != nil {
		t.Fatal(err)
	}
}
