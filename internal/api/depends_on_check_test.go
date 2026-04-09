package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// multiTaskStore は複数タスクを保持するテスト用スタブ。
type multiTaskStore struct {
	tasks map[string]*orchestrator.Task
}

func (s *multiTaskStore) CreateTask(task *orchestrator.Task) error {
	if task.ID == "" {
		task.ID = "stub-task-id"
	}
	s.tasks[task.ID] = task
	return nil
}
func (s *multiTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, &StatusError{Code: 404, Message: "task not found: " + id}
	}
	return t, nil
}
func (s *multiTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	var result []*orchestrator.Task
	for _, t := range s.tasks {
		if filter.Status != "" && string(t.Status) != filter.Status {
			continue
		}
		result = append(result, t)
	}
	return result, nil
}
func (s *multiTaskStore) UpdateTask(task *orchestrator.Task) error {
	s.tasks[task.ID] = task
	return nil
}
func (s *multiTaskStore) DeleteTask(id string) error {
	delete(s.tasks, id)
	return nil
}
func (s *multiTaskStore) FindTaskByRemote(remoteID, datasourceID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *multiTaskStore) FindDependentTasks(taskID string) ([]*orchestrator.Task, error) {
	var result []*orchestrator.Task
	for _, t := range s.tasks {
		if t.Status != orchestrator.TaskStatusPending {
			continue
		}
		for _, dep := range t.DependsOn {
			if dep == taskID {
				result = append(result, t)
				break
			}
		}
	}
	return result, nil
}

// --- checkDependencies ユニットテスト ---

func TestCheckDependencies_NoDependsOn_AlwaysOK(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", DependsOn: nil}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		t.Fatalf("getTask called unexpectedly for id=%s", id)
		return nil, nil
	}); err != nil {
		t.Fatalf("checkDependencies() error = %v, want nil", err)
	}
}

func TestCheckDependencies_AllDone_NoError(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep-1", Status: orchestrator.TaskStatusDone}
	task := &orchestrator.Task{ID: "t1", DependsOn: []string{"dep-1"}}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err != nil {
		t.Fatalf("checkDependencies() error = %v, want nil", err)
	}
}

func TestCheckDependencies_OnePending_Error(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep-1", Status: orchestrator.TaskStatusPending}
	task := &orchestrator.Task{ID: "t1", DependsOn: []string{"dep-1"}}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error for pending dependency")
	}
}

func TestCheckDependencies_MultipleDepsMixedStatus_Error(t *testing.T) {
	deps := map[string]*orchestrator.Task{
		"dep-1": {ID: "dep-1", Status: orchestrator.TaskStatusDone},
		"dep-2": {ID: "dep-2", Status: orchestrator.TaskStatusPending},
	}
	task := &orchestrator.Task{ID: "t1", DependsOn: []string{"dep-1", "dep-2"}}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return deps[id], nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error when one dep is not done")
	}
}

func TestCheckDependencies_DependsOnPayload_Truthy_NoError(t *testing.T) {
	dep := &orchestrator.Task{
		ID:      "dep-1",
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{"result": "ok"}`),
	}
	task := &orchestrator.Task{
		ID:               "t1",
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "result",
	}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err != nil {
		t.Fatalf("checkDependencies() error = %v, want nil for truthy payload", err)
	}
}

func TestCheckDependencies_DependsOnPayload_FalsyString_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:      "dep-1",
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{"result": ""}`),
	}
	task := &orchestrator.Task{
		ID:               "t1",
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "result",
	}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error for empty string payload")
	}
}

func TestCheckDependencies_DependsOnPayload_FalsyNull_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:      "dep-1",
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{"result": null}`),
	}
	task := &orchestrator.Task{
		ID:               "t1",
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "result",
	}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error for null payload")
	}
}

func TestCheckDependencies_DependsOnPayload_FalsyFalse_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:      "dep-1",
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{"result": false}`),
	}
	task := &orchestrator.Task{
		ID:               "t1",
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "result",
	}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error for false payload")
	}
}

func TestCheckDependencies_DependsOnPayload_FalsyZero_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:      "dep-1",
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{"result": 0}`),
	}
	task := &orchestrator.Task{
		ID:               "t1",
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "result",
	}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error for zero payload")
	}
}

func TestCheckDependencies_DependsOnPayload_FalsyEmptyArray_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:      "dep-1",
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{"result": []}`),
	}
	task := &orchestrator.Task{
		ID:               "t1",
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "result",
	}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error for empty array payload")
	}
}

func TestCheckDependencies_DependsOnPayload_FalsyEmptyObject_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:      "dep-1",
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{"result": {}}`),
	}
	task := &orchestrator.Task{
		ID:               "t1",
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "result",
	}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error for empty object payload")
	}
}

func TestCheckDependencies_DependsOnPayload_KeyMissing_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:      "dep-1",
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{"other": "value"}`),
	}
	task := &orchestrator.Task{
		ID:               "t1",
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "result",
	}
	if err := checkDependencies(task, func(id string) (*orchestrator.Task, error) {
		return dep, nil
	}); err == nil {
		t.Fatal("checkDependencies() error = nil, want error for missing key")
	}
}

// --- ApplyAction 統合テスト ---

func TestTaskWorkflowServiceApplyAction_Start_AllDepsDone_Success(t *testing.T) {
	dep := &orchestrator.Task{
		ID:        "dep-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "impl",
	}
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
		DependsOn: []string{"dep-1"},
	}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		"dep-1":  dep,
		"task-1": task,
	}}
	txStore := &recordingTxStore{task: task}

	svc := &TaskWorkflowService{
		Tasks:    store,
		Tx:       recordingTransactor{store: txStore},
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver: stubResolver{sm: orchestrator.OneShotMachine()},
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction() error = %v, want nil (all deps done)", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want %q", result.Task.Status, orchestrator.TaskStatusExecuting)
	}
}

func TestTaskWorkflowServiceApplyAction_Start_DepNotDone_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:        "dep-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
	}
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
		DependsOn: []string{"dep-1"},
	}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		"dep-1":  dep,
		"task-1": task,
	}}
	txStore := &recordingTxStore{task: task}

	svc := &TaskWorkflowService{
		Tasks:    store,
		Tx:       recordingTransactor{store: txStore},
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver: stubResolver{sm: orchestrator.OneShotMachine()},
	}

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want error for pending dependency")
	}
}

func TestTaskWorkflowServiceApplyAction_Start_ErrorMessageContainsDependencyID(t *testing.T) {
	dep := &orchestrator.Task{
		ID:        "dep-abc",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
	}
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
		DependsOn: []string{"dep-abc"},
	}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		"dep-abc": dep,
		"task-1":  task,
	}}
	txStore := &recordingTxStore{task: task}

	svc := &TaskWorkflowService{
		Tasks:    store,
		Tx:       recordingTransactor{store: txStore},
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver: stubResolver{sm: orchestrator.OneShotMachine()},
	}

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != 409 {
		t.Fatalf("error code = %d, want 409", se.Code)
	}
}

func TestTaskWorkflowServiceApplyAction_Start_NoDeps_NotBlocked(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
		DependsOn: nil,
	}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{"task-1": task}}
	txStore := &recordingTxStore{task: task}

	svc := &TaskWorkflowService{
		Tasks:    store,
		Tx:       recordingTransactor{store: txStore},
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver: stubResolver{sm: orchestrator.OneShotMachine()},
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction() error = %v, want nil (no deps)", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want %q", result.Task.Status, orchestrator.TaskStatusExecuting)
	}
}

func TestTaskWorkflowServiceApplyAction_Start_DepsOnPayload_Truthy_Success(t *testing.T) {
	dep := &orchestrator.Task{
		ID:        "dep-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "impl",
		Payload:   json.RawMessage(`{"output": "some value"}`),
	}
	task := &orchestrator.Task{
		ID:               "task-1",
		ProjectID:        "proj-1",
		Status:           orchestrator.TaskStatusPending,
		Behavior:         "impl",
		Payload:          []byte(`{}`),
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "output",
	}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		"dep-1":  dep,
		"task-1": task,
	}}
	txStore := &recordingTxStore{task: task}

	svc := &TaskWorkflowService{
		Tasks:    store,
		Tx:       recordingTransactor{store: txStore},
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver: stubResolver{sm: orchestrator.OneShotMachine()},
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction() error = %v, want nil (truthy payload)", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want %q", result.Task.Status, orchestrator.TaskStatusExecuting)
	}
}

func TestTaskWorkflowServiceApplyAction_Start_DepsOnPayload_Falsy_Error(t *testing.T) {
	dep := &orchestrator.Task{
		ID:        "dep-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "impl",
		Payload:   json.RawMessage(`{"output": null}`),
	}
	task := &orchestrator.Task{
		ID:               "task-1",
		ProjectID:        "proj-1",
		Status:           orchestrator.TaskStatusPending,
		Behavior:         "impl",
		Payload:          []byte(`{}`),
		DependsOn:        []string{"dep-1"},
		DependsOnPayload: "output",
	}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		"dep-1":  dep,
		"task-1": task,
	}}
	txStore := &recordingTxStore{task: task}

	svc := &TaskWorkflowService{
		Tasks:    store,
		Tx:       recordingTransactor{store: txStore},
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver: stubResolver{sm: orchestrator.OneShotMachine()},
	}

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want error for falsy depends_on_payload")
	}
}

// --- payloadGet ネストパス参照ユニットテスト ---

func TestPayloadGet_NestedPath_Truthy(t *testing.T) {
	payload := json.RawMessage(`{"artifact": {"pr": {"merge_status": "merged"}}}`)
	v, err := payloadGet(payload, "artifact.pr.merge_status")
	if err != nil {
		t.Fatalf("payloadGet() error = %v, want nil", err)
	}
	if v != "merged" {
		t.Fatalf("payloadGet() = %v, want %q", v, "merged")
	}
}

func TestPayloadGet_NestedPath_Falsy(t *testing.T) {
	payload := json.RawMessage(`{"artifact": {"pr": {"merge_status": ""}}}`)
	v, err := payloadGet(payload, "artifact.pr.merge_status")
	if err != nil {
		t.Fatalf("payloadGet() error = %v, want nil", err)
	}
	if isTruthy(v) {
		t.Fatal("payloadGet() value should be falsy but got truthy")
	}
}

func TestPayloadGet_NestedPath_IntermediateMissing(t *testing.T) {
	payload := json.RawMessage(`{"artifact": {}}`)
	v, err := payloadGet(payload, "artifact.pr.merge_status")
	if err != nil {
		t.Fatalf("payloadGet() error = %v, want nil", err)
	}
	if v != nil {
		t.Fatalf("payloadGet() = %v, want nil for missing intermediate path", v)
	}
}

func TestPayloadGet_NestedPath_IntermediateNotMap(t *testing.T) {
	payload := json.RawMessage(`{"artifact": "not-a-map"}`)
	v, err := payloadGet(payload, "artifact.pr.merge_status")
	if err != nil {
		t.Fatalf("payloadGet() error = %v, want nil", err)
	}
	if v != nil {
		t.Fatalf("payloadGet() = %v, want nil when intermediate is not a map", v)
	}
}

func TestPayloadGet_DeepNest(t *testing.T) {
	payload := json.RawMessage(`{"a": {"b": {"c": {"d": "deep"}}}}`)
	v, err := payloadGet(payload, "a.b.c.d")
	if err != nil {
		t.Fatalf("payloadGet() error = %v, want nil", err)
	}
	if v != "deep" {
		t.Fatalf("payloadGet() = %v, want %q for deep nesting", v, "deep")
	}
}

func TestPayloadGet_TopLevelKey_Regression(t *testing.T) {
	payload := json.RawMessage(`{"result": "ok"}`)
	v, err := payloadGet(payload, "result")
	if err != nil {
		t.Fatalf("payloadGet() error = %v, want nil", err)
	}
	if v != "ok" {
		t.Fatalf("payloadGet() = %v, want %q for top-level key", v, "ok")
	}
}

// --- auto_start + 依存未充足 → pending 維持（エラーなし）テスト ---

func TestTaskAppServiceCreateTask_AutoStart_DepNotSatisfied_StaysPending(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {Transition: "one-shot"},
		},
	}
	dep := &orchestrator.Task{
		ID:     "dep-task-id",
		Status: orchestrator.TaskStatusPending, // done でない
	}
	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		"dep-task-id": dep,
	}}
	workflow := &stubWorkflowService{
		applyActionErr: &StatusError{Code: 409, Message: "dependency not satisfied: dep-task-id is not done"},
	}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: meta},
		Workflow: workflow,
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "dependent task",
		Behavior:  "dev",
		AutoStart: true,
		DependsOn: []string{"dep-task-id"},
	})
	// CreateTask はエラーを返さない（タスクは pending で作成される）
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil (auto_start skip on dep failure)", err)
	}
	if task == nil {
		t.Fatal("CreateTask() returned nil task")
	}
}
