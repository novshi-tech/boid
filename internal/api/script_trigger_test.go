package api

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// mapTaskStore is a TaskStore backed by an in-memory map for integration tests.
type mapTaskStore struct {
	tasks       map[string]*orchestrator.Task
	createdIDs  []string
}

func newMapTaskStore(initial ...*orchestrator.Task) *mapTaskStore {
	s := &mapTaskStore{tasks: make(map[string]*orchestrator.Task)}
	for _, t := range initial {
		s.tasks[t.ID] = t
	}
	return s
}

func (s *mapTaskStore) CreateTask(task *orchestrator.Task) error {
	if task.ID == "" {
		task.ID = "generated-" + task.Title
	}
	s.tasks[task.ID] = task
	s.createdIDs = append(s.createdIDs, task.ID)
	return nil
}
func (s *mapTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, &StatusError{Code: 404, Message: "task not found: " + id}
	}
	return t, nil
}
func (s *mapTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *mapTaskStore) UpdateTask(task *orchestrator.Task) error {
	s.tasks[task.ID] = task
	return nil
}
func (s *mapTaskStore) DeleteTask(id string) error { return nil }
func (s *mapTaskStore) FindTaskByRemote(remoteID, datasourceID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *mapTaskStore) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *mapTaskStore) FindDependentTasks(taskID string) ([]*orchestrator.Task, error) {
	return nil, nil
}

func TestRunDispatchLoop_ScriptTriggeredOnTaskDone(t *testing.T) {
	parentTask := &orchestrator.Task{
		ID:        "parent-1",
		ProjectID: "proj-1",
		Title:     "parent task",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
		Payload:   []byte(`{}`),
	}

	taskStore := newMapTaskStore(parentTask)

	txStore := &recordingTxStore{task: parentTask}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
		Scripts: []orchestrator.Script{
			{
				ID:          "notify",
				Kit:         "my-kit",
				Description: "Sends notification",
				On:          []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone},
			},
		},
	}

	svc := &TaskWorkflowService{
		Tasks: taskStore,
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: meta},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				FinalPayload: parentTask.Payload,
			},
		},
	}

	svc.runDispatchLoop(
		context.Background(),
		parentTask,
		meta,
		orchestrator.DefaultMachine(),
	)

	// Verify a script task was created
	if len(taskStore.createdIDs) != 1 {
		t.Fatalf("expected 1 script task created, got %d", len(taskStore.createdIDs))
	}

	scriptTask := taskStore.tasks[taskStore.createdIDs[0]]
	if scriptTask == nil {
		t.Fatal("script task not found in store")
	}
	if !scriptTask.Ephemeral {
		t.Error("script task Ephemeral should be true")
	}
	if !scriptTask.Readonly {
		t.Error("script task Readonly should be true")
	}
	if scriptTask.Title != "script: my-kit/notify" {
		t.Errorf("script task Title = %q, want %q", scriptTask.Title, "script: my-kit/notify")
	}
	if scriptTask.Behavior != "_script:my-kit/notify" {
		t.Errorf("script task Behavior = %q, want %q", scriptTask.Behavior, "_script:my-kit/notify")
	}
	if scriptTask.ParentID != parentTask.ID {
		t.Errorf("script task ParentID = %q, want %q", scriptTask.ParentID, parentTask.ID)
	}
	if scriptTask.ProjectID != parentTask.ProjectID {
		t.Errorf("script task ProjectID = %q, want %q", scriptTask.ProjectID, parentTask.ProjectID)
	}
}

func TestRunDispatchLoop_EphemeralTaskDoesNotFireScripts(t *testing.T) {
	ephemeralTask := &orchestrator.Task{
		ID:        "ephemeral-1",
		ProjectID: "proj-1",
		Title:     "ephemeral task",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "notify",
		Ephemeral: true,
		Payload:   []byte(`{}`),
	}

	taskStore := newMapTaskStore(ephemeralTask)

	txStore := &recordingTxStore{task: ephemeralTask}
	meta := &orchestrator.ProjectMeta{
		Scripts: []orchestrator.Script{
			{
				ID: "chain",
				On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone},
			},
		},
	}

	svc := &TaskWorkflowService{
		Tasks: taskStore,
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: meta},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				FinalPayload: ephemeralTask.Payload,
			},
		},
	}

	svc.runDispatchLoop(
		context.Background(),
		ephemeralTask,
		meta,
		orchestrator.DefaultMachine(),
	)

	if len(taskStore.createdIDs) != 0 {
		t.Errorf("ephemeral task completion should not create script tasks, got %d", len(taskStore.createdIDs))
	}
}

func TestRunDispatchLoop_ScriptNotFiredOnAbort(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Title:     "task",
		Status:    orchestrator.TaskStatusAborted,
		Behavior:  "dev",
		Payload:   []byte(`{}`),
	}

	taskStore := newMapTaskStore(task)
	txStore := &recordingTxStore{task: task}
	meta := &orchestrator.ProjectMeta{
		Scripts: []orchestrator.Script{
			{
				ID: "notify",
				On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone},
			},
		},
	}

	svc := &TaskWorkflowService{
		Tasks: taskStore,
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: meta},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				FinalPayload: task.Payload,
			},
		},
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		meta,
		orchestrator.DefaultMachine(),
	)

	if len(taskStore.createdIDs) != 0 {
		t.Errorf("aborted task should not fire task_done scripts, got %d created", len(taskStore.createdIDs))
	}
}
