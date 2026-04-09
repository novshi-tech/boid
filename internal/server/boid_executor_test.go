package server

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type capturingTaskStore struct {
	created []*orchestrator.Task
}

// executorMetaStub provides a minimal MetaStore for boid executor tests.
type executorMetaStub struct {
	meta *orchestrator.ProjectMeta
}

func (s executorMetaStub) Get(_ string) (*orchestrator.ProjectMeta, bool) {
	if s.meta == nil {
		return nil, false
	}
	return s.meta, true
}

func (s *capturingTaskStore) CreateTask(task *orchestrator.Task) error {
	task.ID = "task-1"
	task.Status = orchestrator.TaskStatusPending
	s.created = append(s.created, task)
	return nil
}

func (s *capturingTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	return nil, nil
}

func (s *capturingTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}

func (s *capturingTaskStore) UpdateTask(task *orchestrator.Task) error {
	return nil
}
func (s *capturingTaskStore) DeleteTask(id string) error { return nil }
func (s *capturingTaskStore) FindTaskByRemote(remoteID, datasourceID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *capturingTaskStore) FindDependentTasks(taskID string) ([]*orchestrator.Task, error) {
	return nil, nil
}

func TestBoidBuiltinExecutor_EnforcesWorkspaceScope(t *testing.T) {
	store := &capturingTaskStore{}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {Transition: "one-shot"},
		},
	}}
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: store, Meta: meta},
	}
	ctx := sandbox.TokenContext{
		ProjectID:         "proj-1",
		WorkspaceID:       "ws-1",
		AllowedProjectIDs: []string{"proj-1", "proj-2"},
	}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:       sandbox.BoidOpTaskCreate,
		Title:    "same workspace",
		Behavior: "dev",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("self project create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 || store.created[0].ProjectID != "proj-1" {
		t.Fatalf("created tasks = %+v, want current project", store.created)
	}

	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskCreate,
		ProjectID: "proj-2",
		Title:     "peer workspace",
		Behavior:  "dev",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("peer project create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 2 || store.created[1].ProjectID != "proj-2" {
		t.Fatalf("created tasks = %+v, want peer project", store.created)
	}

	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskCreate,
		ProjectID: "proj-3",
		Title:     "cross workspace",
		Behavior:  "dev",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("cross-workspace create should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 2 {
		t.Fatalf("cross-workspace create should not reach task store, created=%d", len(store.created))
	}
}
