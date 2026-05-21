package api_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// helper: task を aborted 状態にする
func abortTask(t *testing.T, ts *testutil.TestServer, taskID string) {
	t.Helper()
	if err := ts.Client.Do("POST", "/api/tasks/"+taskID+"/actions", map[string]any{"type": "abort"}, nil); err != nil {
		t.Fatalf("abort task: %v", err)
	}
}

func TestRerunTask_AbortedToPending(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-1", "Rerun Project 1")

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-1",
		"title":      "Rerun Me",
		"behavior":   "planning",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	abortTask(t, ts, task.ID)

	// rerun
	var result orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/rerun", map[string]any{"auto_start": false}, &result); err != nil {
		t.Fatalf("rerun task: %v", err)
	}

	if result.Status != orchestrator.TaskStatusPending {
		t.Errorf("Status = %q, want %q", result.Status, orchestrator.TaskStatusPending)
	}
}

func TestRerunTask_PreservesID(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-2", "Rerun Project 2")

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-2",
		"title":      "Preserve ID",
		"behavior":   "planning",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	abortTask(t, ts, task.ID)

	var result orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/rerun", map[string]any{"auto_start": false}, &result); err != nil {
		t.Fatalf("rerun task: %v", err)
	}

	if result.ID != task.ID {
		t.Errorf("ID changed: got %q, want %q", result.ID, task.ID)
	}
}

func TestRerunTask_ClearsPayload(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-3", "Rerun Project 3")

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-3",
		"title":      "Clear Payload",
		"behavior":   "planning",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// payload を更新してから abort
	if err := ts.Client.Do("PATCH", "/api/tasks/"+task.ID, map[string]any{
		"title":   "Clear Payload",
		"payload": json.RawMessage(`{"artifact":{"url":"https://example.com"},"verification":{"gate":{"source_state":"executing","findings":[]}}}`),
	}, nil); err != nil {
		t.Fatalf("patch task: %v", err)
	}

	abortTask(t, ts, task.ID)

	var result orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/rerun", map[string]any{"auto_start": false}, &result); err != nil {
		t.Fatalf("rerun task: %v", err)
	}

	// payload は空 (または instructions のみ) になるはず
	var payloadMap map[string]json.RawMessage
	if err := json.Unmarshal(result.Payload, &payloadMap); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := payloadMap["artifact"]; ok {
		t.Error("artifact should be cleared after rerun")
	}
	if _, ok := payloadMap["verification"]; ok {
		t.Error("verification should be cleared after rerun")
	}
}

func TestRerunTask_PreservesInstructions(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-ins", "Rerun Project Ins")

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id":   "rerun-proj-ins",
		"title":        "Preserve Instructions",
		"behavior":     "planning",
		"instructions": json.RawMessage(`{"main":{"agent":"claude-code","message":"do stuff","type":"execution"}}`),
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// dirty な artifact を入れてから abort → rerun で artifact はクリアされるが instructions は保持される
	if err := ts.Client.Do("PATCH", "/api/tasks/"+task.ID, map[string]any{
		"payload": json.RawMessage(`{"artifact":{"url":"old"}}`),
	}, nil); err != nil {
		t.Fatalf("patch task: %v", err)
	}

	abortTask(t, ts, task.ID)

	var result orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/rerun", map[string]any{"auto_start": false}, &result); err != nil {
		t.Fatalf("rerun task: %v", err)
	}

	if len(result.Instructions) == 0 {
		t.Errorf("instructions should be preserved after rerun, got %v", result.Instructions)
	}
	var payloadMap map[string]json.RawMessage
	if err := json.Unmarshal(result.Payload, &payloadMap); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := payloadMap["artifact"]; ok {
		t.Error("artifact should be cleared after rerun")
	}
}

func TestRerunTask_NotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	err := ts.Client.Do("POST", "/api/tasks/nonexistent-id/rerun", map[string]any{"auto_start": false}, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent task, got nil")
	}
}

func TestRerunTask_WrongStatus_Pending(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-ws1", "Rerun WS1")

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-ws1",
		"title":      "Wrong Status Pending",
		"behavior":   "planning",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// pending タスクに rerun → 409
	err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/rerun", map[string]any{"auto_start": false}, nil)
	if err == nil {
		t.Fatal("expected 409 for pending task, got nil")
	}
}

func TestRerunTask_WrongStatus_Executing(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-ws2", "Rerun WS2")

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-ws2",
		"title":      "Wrong Status Executing",
		"behavior":   "planning",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// start → executing
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/actions", map[string]any{"type": "start"}, nil); err != nil {
		t.Fatalf("start task: %v", err)
	}

	// executing タスクに rerun → 409
	err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/rerun", map[string]any{"auto_start": false}, nil)
	if err == nil {
		t.Fatal("expected 409 for executing task, got nil")
	}
}

func TestRerunTask_PreservesActionHistory(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-hist", "Rerun Hist")

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-hist",
		"title":      "Preserve History",
		"behavior":   "planning",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	abortTask(t, ts, task.ID)

	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/rerun", map[string]any{"auto_start": false}, nil); err != nil {
		t.Fatalf("rerun task: %v", err)
	}

	// detail を取得してアクション履歴を確認
	var detail struct {
		Actions []orchestrator.Action `json:"actions"`
	}
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID+"/detail", nil, &detail); err != nil {
		t.Fatalf("get task detail: %v", err)
	}

	// abort アクションが履歴に残っているはず
	hasAbort := false
	for _, a := range detail.Actions {
		if a.Type == "abort" {
			hasAbort = true
		}
	}
	if !hasAbort {
		t.Error("abort action should remain in history after rerun")
	}
}

func TestRerunTask_DownstreamUntouched(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-ds", "Rerun DS")

	// 親タスク
	var parent orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-ds",
		"title":      "Parent Task",
		"behavior":   "planning",
	}, &parent); err != nil {
		t.Fatalf("create parent task: %v", err)
	}

	// 下流タスク (親とは独立した pending タスク)
	var downstream orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-ds",
		"title":      "Downstream Task",
		"behavior":   "planning",
	}, &downstream); err != nil {
		t.Fatalf("create downstream task: %v", err)
	}

	abortTask(t, ts, parent.ID)

	if err := ts.Client.Do("POST", "/api/tasks/"+parent.ID+"/rerun", map[string]any{"auto_start": false}, nil); err != nil {
		t.Fatalf("rerun parent: %v", err)
	}

	// 下流タスクの状態が変わっていないこと
	var ds orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+downstream.ID, nil, &ds); err != nil {
		t.Fatalf("get downstream task: %v", err)
	}
	if ds.Status != orchestrator.TaskStatusPending {
		t.Errorf("downstream status changed: got %q, want pending", ds.Status)
	}
}

func TestRerunTask_AutoStart(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "rerun-proj-as", "Rerun AutoStart")

	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "rerun-proj-as",
		"title":      "AutoStart Rerun",
		"behavior":   "planning",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	abortTask(t, ts, task.ID)

	var result orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/rerun", map[string]any{"auto_start": true}, &result); err != nil {
		t.Fatalf("rerun task with auto_start: %v", err)
	}

	if result.Status != orchestrator.TaskStatusExecuting {
		t.Errorf("Status = %q, want %q", result.Status, orchestrator.TaskStatusExecuting)
	}
}
