package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestApplyAction_URLDerivedProjectHydrateFailureIsHardError(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "url-abc123",
		Title:     "start task",
		Status:    orchestrator.TaskStatusPending,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: &recordingTxStore{}},
		Meta: stubMetaStore{
			// The bare cached meta for a no-YAML project is minimal (ID+Name
			// only, see synthesizeNoYAMLProjectMeta) — no TaskBehaviors.
			meta:       &orchestrator.ProjectMeta{ID: "url-abc123", Name: "myrepo"},
			hydrateErr: errors.New("workspace store unavailable"),
		},
	}

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want a hard error for a url- prefixed project whose workspace hydration failed")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("ApplyAction() error type = %T, want *StatusError", err)
	}
	if !strings.Contains(statusErr.Message, "url-abc123") {
		t.Errorf("error message = %q, want it to mention the project id", statusErr.Message)
	}
	if !strings.Contains(statusErr.Message, "workspace store unavailable") {
		t.Errorf("error message = %q, want it to include the underlying hydrate error", statusErr.Message)
	}
}

func TestApplyAction_URLDerivedProjectDegradedSuccessIsAlsoHardError(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "url-abc123",
		Title:     "start task",
		Status:    orchestrator.TaskStatusPending,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: &recordingTxStore{}},
		Meta: stubMetaStore{
			// hydrateErr is nil (GetWithWorkspace "succeeds"), but the
			// returned meta has zero TaskBehaviors — the degraded-window
			// shape, not a genuine merge.
			meta: &orchestrator.ProjectMeta{ID: "url-abc123", Name: "myrepo"},
		},
	}

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want a hard error: hydrate 'succeeded' but supplied zero task_behaviors for a no-YAML project")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("ApplyAction() error type = %T, want *StatusError", err)
	}
	if !strings.Contains(statusErr.Message, "url-abc123") {
		t.Errorf("error message = %q, want it to mention the project id", statusErr.Message)
	}
}

// TestApplyAction_URLDerivedProjectSuccessfulMergeDispatches confirms the
// happy path is untouched by the two guards above: a no-YAML project whose
// GetWithWorkspace call actually merges in workspace-default task_behaviors
// dispatches normally, with no hard error.
func TestApplyAction_URLDerivedProjectSuccessfulMergeDispatches(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "url-abc123",
		Title:     "start task",
		Status:    orchestrator.TaskStatusPending,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}

	probe := newDispatchContextProbe()
	defer close(probe.release)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: &recordingTxStore{}},
		Meta: stubMetaStore{
			meta: &orchestrator.ProjectMeta{ID: "url-abc123", Name: "myrepo"},
			hydrated: &orchestrator.ProjectMeta{
				ID: "url-abc123", Name: "myrepo",
				TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}},
			},
		},
		Coordinator: probe,
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction() error = %v, want nil", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want %q", result.Task.Status, orchestrator.TaskStatusExecuting)
	}
}

func TestApplyActionRegistryProjectHydrationFailureRejectsDispatch(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Title:     "start task",
		Status:    orchestrator.TaskStatusPending,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}

	probe := newDispatchContextProbe()
	defer close(probe.release)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: &recordingTxStore{}},
		Meta: stubMetaStore{
			meta:       &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}},
			hydrateErr: errors.New("workspace.yaml: corrupt"),
		},
		Coordinator: probe,
	}

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err == nil || !strings.Contains(err.Error(), "workspace.yaml: corrupt") {
		t.Fatalf("missing workspace must reject dispatch: %v", err)
	}

}
