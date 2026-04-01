package orchestrator

import "testing"

type stubProjectCatalog struct {
	projects []*Project
}

func (s stubProjectCatalog) GetProject(id string) (*Project, error) {
	for _, project := range s.projects {
		if project.ID == id {
			return project, nil
		}
	}
	return nil, nil
}

func (s stubProjectCatalog) ListProjects() ([]*Project, error) {
	return s.projects, nil
}

func TestDispatchPlannerCollectWorkspaceDirs_UsesProjectWorkspaceMembership(t *testing.T) {
	planner := &DispatchPlanner{
		Projects: stubProjectCatalog{
			projects: []*Project{
				{ID: "proj-1", WorkspaceID: "ws-1", WorkDir: "/workspace/proj-1"},
				{ID: "proj-2", WorkspaceID: "ws-1", WorkDir: "/workspace/proj-2"},
				{ID: "proj-3", WorkspaceID: "ws-2", WorkDir: "/workspace/proj-3"},
			},
		},
	}

	dirs, err := planner.collectWorkspaceDirs("ws-1", "proj-1")
	if err != nil {
		t.Fatalf("collectWorkspaceDirs: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("expected 1 peer workspace dir, got %d", len(dirs))
	}
	if dirs["proj-2"] != "/workspace/proj-2" {
		t.Fatalf("peer workspace dir = %#v", dirs)
	}
	if _, ok := dirs["proj-3"]; ok {
		t.Fatalf("unexpected workspace dir from another workspace: %#v", dirs)
	}
}
