package dispatcher

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type fakeProjectLookup struct {
	projects []*orchestrator.Project
}

func (f fakeProjectLookup) GetProject(id string) (*orchestrator.Project, error) {
	for _, p := range f.projects {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, nil
}

func (f fakeProjectLookup) ListProjects() ([]*orchestrator.Project, error) {
	return f.projects, nil
}

func TestRunnerResolveWorkspacePeers_SameWorkspaceExcludesSelfAndOtherWorkspaces(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1", WorkDir: "/workspace/proj-1"},
			{ID: "proj-2", WorkspaceID: "ws-1", WorkDir: "/workspace/proj-2"},
			{ID: "proj-3", WorkspaceID: "ws-2", WorkDir: "/workspace/proj-3"},
		}},
	}

	peers := r.resolveWorkspacePeers("ws-1", "proj-1")
	if len(peers) != 1 {
		t.Fatalf("peers = %#v, want 1 entry", peers)
	}
	if peers["proj-2"] != "/workspace/proj-2" {
		t.Fatalf("peers[proj-2] = %q, want /workspace/proj-2", peers["proj-2"])
	}
	if _, ok := peers["proj-1"]; ok {
		t.Fatalf("peers should not contain self: %#v", peers)
	}
	if _, ok := peers["proj-3"]; ok {
		t.Fatalf("peers should not contain other workspace: %#v", peers)
	}
}

// Gate jobs historically passed Visibility.WorkspacePeers=nil, which caused
// AllowedProjectIDs to shrink to self-only and blocked `boid task create` on
// peer projects. Dispatcher now resolves peers independently from the
// project catalog, so gate jobs see the full peer allowlist too.
func TestRunnerResolveWorkspacePeers_GateStillSeesPeers(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "boid", WorkspaceID: "boid", WorkDir: "/work/boid"},
			{ID: "boid-kits", WorkspaceID: "boid", WorkDir: "/work/boid-kits"},
		}},
	}

	peers := r.resolveWorkspacePeers("boid", "boid")
	allowed := allowedProjectIDs("boid", peers)
	if len(allowed) != 2 {
		t.Fatalf("allowed = %#v, want [boid boid-kits]", allowed)
	}
	if allowed[0] != "boid" || allowed[1] != "boid-kits" {
		t.Fatalf("allowed = %#v, want [boid boid-kits]", allowed)
	}
}

func TestRunnerResolveWorkspacePeers_NilWhenWorkspaceEmpty(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "", WorkDir: "/workspace/proj-1"},
		}},
	}
	if peers := r.resolveWorkspacePeers("", "proj-1"); peers != nil {
		t.Fatalf("peers = %#v, want nil when workspace is empty", peers)
	}
}
