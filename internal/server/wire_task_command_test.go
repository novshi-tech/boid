package server

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubProjectMeta implements the anonymous Meta interface in api.ProjectAppService.
type stubProjectMeta struct{}

func (s *stubProjectMeta) Load(workDir string) (*orchestrator.ProjectMeta, error) { return nil, nil }
func (s *stubProjectMeta) Get(id string) (*orchestrator.ProjectMeta, bool)        { return nil, false }
func (s *stubProjectMeta) Remove(id string)                                       {}
func (s *stubProjectMeta) LoadAll(projects []*orchestrator.Project) []error       { return nil }

// capturingJobDispatcher captures the JobSpec passed to Dispatch.
type capturingJobDispatcher struct {
	spec *orchestrator.JobSpec
}

func (c *capturingJobDispatcher) Dispatch(ctx context.Context, spec *orchestrator.JobSpec, cleanup orchestrator.CleanupFunc) (string, error) {
	c.spec = spec
	return "job-id", nil
}

func newTaskCommandAdapter(task *orchestrator.Task) (*taskCommandDispatcherAdapter, *capturingJobDispatcher) {
	ts := &capturingTaskStore{created: []*orchestrator.Task{task}}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			task.Behavior: {
				Commands: map[string]orchestrator.CommandSpec{
					"discuss": {ResolvedCommand: []string{"claude", "--print"}},
				},
			},
		},
	}}
	taskSvc := &api.TaskAppService{Tasks: ts, Meta: meta}
	projSvc := &api.ProjectAppService{
		Projects: stubProjectRepo{projects: []*orchestrator.Project{
			{ID: task.ProjectID, WorkDir: "/tmp"},
		}},
		Meta: &stubProjectMeta{},
	}
	cd := &capturingJobDispatcher{}
	adapter := &taskCommandDispatcherAdapter{
		taskSvc:    taskSvc,
		projectSvc: projSvc,
		runner:     cd,
	}
	return adapter, cd
}

func TestTaskCommandAdapter_SetsTaskSnapshotAndPayload(t *testing.T) {
	payload := json.RawMessage(`{"key":"value"}`)
	task := &orchestrator.Task{
		ID:          "task-1",
		ProjectID:   "proj-1",
		Title:       "Test Task",
		Status:      orchestrator.TaskStatusExecuting,
		Behavior:    "dev",
		Description: "test description",
		Payload:     payload,
	}

	adapter, cd := newTaskCommandAdapter(task)

	result, err := adapter.ExecuteTaskBehaviorCommand(context.Background(), "task-1", "discuss")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	spec := cd.spec
	if spec == nil {
		t.Fatal("Dispatch was not called")
	}
	if spec.Task == nil {
		t.Fatal("spec.Task is nil")
	}
	if spec.Task.ID != task.ID {
		t.Errorf("spec.Task.ID = %q, want %q", spec.Task.ID, task.ID)
	}
	if spec.Task.Title != task.Title {
		t.Errorf("spec.Task.Title = %q, want %q", spec.Task.Title, task.Title)
	}
	if spec.Task.Description != task.Description {
		t.Errorf("spec.Task.Description = %q, want %q", spec.Task.Description, task.Description)
	}
	if !bytes.Equal(spec.PrimaryInput, payload) {
		t.Errorf("spec.PrimaryInput = %q, want %q", spec.PrimaryInput, payload)
	}
}

func TestTaskCommandAdapter_EmptyPayloadLeavesNilPrimaryInput(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-2",
		ProjectID: "proj-1",
		Title:     "No Payload Task",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "dev",
	}

	adapter, cd := newTaskCommandAdapter(task)

	if _, err := adapter.ExecuteTaskBehaviorCommand(context.Background(), "task-2", "discuss"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cd.spec.PrimaryInput != nil {
		t.Errorf("PrimaryInput should be nil for task with no payload, got %q", cd.spec.PrimaryInput)
	}
}
