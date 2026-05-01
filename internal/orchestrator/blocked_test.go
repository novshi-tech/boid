package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// makeTaskMap は与えられたタスクスライスを ID でひけるマップに変換する。
func makeTaskMap(tasks ...*orchestrator.Task) map[string]*orchestrator.Task {
	m := make(map[string]*orchestrator.Task, len(tasks))
	for _, t := range tasks {
		m[t.ID] = t
	}
	return m
}

func TestComputeTaskBlocked_NoDependsOn_NotBlocked(t *testing.T) {
	task := &orchestrator.Task{
		ID:     "t1",
		Status: orchestrator.TaskStatusPending,
	}
	if orchestrator.ComputeTaskBlocked(task, makeTaskMap(task)) {
		t.Error("pending task with no depends_on should not be blocked")
	}
}

func TestComputeTaskBlocked_DepDone_NotBlocked(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", Status: orchestrator.TaskStatusDone}
	task := &orchestrator.Task{
		ID:        "t1",
		Status:    orchestrator.TaskStatusPending,
		DependsOn: []string{"dep"},
	}
	if orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep, task)) {
		t.Error("pending task with done dep should not be blocked")
	}
}

func TestComputeTaskBlocked_DepPending_Blocked(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", Status: orchestrator.TaskStatusPending}
	task := &orchestrator.Task{
		ID:        "t1",
		Status:    orchestrator.TaskStatusPending,
		DependsOn: []string{"dep"},
	}
	if !orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep, task)) {
		t.Error("pending task with pending dep should be blocked")
	}
}

func TestComputeTaskBlocked_DepExecuting_Blocked(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", Status: orchestrator.TaskStatusExecuting}
	task := &orchestrator.Task{
		ID:        "t1",
		Status:    orchestrator.TaskStatusPending,
		DependsOn: []string{"dep"},
	}
	if !orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep, task)) {
		t.Error("pending task with executing dep should be blocked")
	}
}

func TestComputeTaskBlocked_DepNotInMap_NotBlocked(t *testing.T) {
	// dep が削除済み等でマップにない場合はブロックとみなさない
	task := &orchestrator.Task{
		ID:        "t1",
		Status:    orchestrator.TaskStatusPending,
		DependsOn: []string{"deleted-dep"},
	}
	if orchestrator.ComputeTaskBlocked(task, makeTaskMap(task)) {
		t.Error("pending task with missing dep should not be blocked")
	}
}

func TestComputeTaskBlocked_NonPending_NeverBlocked(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", Status: orchestrator.TaskStatusPending}
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	}
	for _, s := range statuses {
		task := &orchestrator.Task{
			ID:        "t1",
			Status:    s,
			DependsOn: []string{"dep"},
		}
		if orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep, task)) {
			t.Errorf("status %q: non-pending task should never be blocked", s)
		}
	}
}

func TestComputeTaskBlocked_DependsOnPayload_ConditionMet_NotBlocked(t *testing.T) {
	payload := json.RawMessage(`{"artifact":{"pr":{"merged":true}}}`)
	dep := &orchestrator.Task{
		ID:      "dep",
		Status:  orchestrator.TaskStatusDone,
		Payload: payload,
	}
	task := &orchestrator.Task{
		ID:               "t1",
		Status:           orchestrator.TaskStatusPending,
		DependsOn:        []string{"dep"},
		DependsOnPayload: "artifact.pr.merged",
	}
	if orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep, task)) {
		t.Error("pending task with truthy DependsOnPayload should not be blocked")
	}
}

func TestComputeTaskBlocked_DependsOnPayload_ConditionNotMet_Blocked(t *testing.T) {
	payload := json.RawMessage(`{"artifact":{"pr":{"merged":false}}}`)
	dep := &orchestrator.Task{
		ID:      "dep",
		Status:  orchestrator.TaskStatusDone,
		Payload: payload,
	}
	task := &orchestrator.Task{
		ID:               "t1",
		Status:           orchestrator.TaskStatusPending,
		DependsOn:        []string{"dep"},
		DependsOnPayload: "artifact.pr.merged",
	}
	if !orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep, task)) {
		t.Error("pending task with falsy DependsOnPayload should be blocked")
	}
}

func TestComputeTaskBlocked_DependsOnPayload_KeyMissing_Blocked(t *testing.T) {
	payload := json.RawMessage(`{}`)
	dep := &orchestrator.Task{
		ID:      "dep",
		Status:  orchestrator.TaskStatusDone,
		Payload: payload,
	}
	task := &orchestrator.Task{
		ID:               "t1",
		Status:           orchestrator.TaskStatusPending,
		DependsOn:        []string{"dep"},
		DependsOnPayload: "artifact.pr.merged",
	}
	if !orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep, task)) {
		t.Error("pending task with missing DependsOnPayload key should be blocked")
	}
}

func TestComputeTaskBlocked_MultipleDeps_OneNotDone_Blocked(t *testing.T) {
	dep1 := &orchestrator.Task{ID: "dep1", Status: orchestrator.TaskStatusDone}
	dep2 := &orchestrator.Task{ID: "dep2", Status: orchestrator.TaskStatusPending}
	task := &orchestrator.Task{
		ID:        "t1",
		Status:    orchestrator.TaskStatusPending,
		DependsOn: []string{"dep1", "dep2"},
	}
	if !orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep1, dep2, task)) {
		t.Error("pending task with one non-done dep should be blocked")
	}
}

func TestComputeTaskBlocked_MultipleDeps_AllDone_NotBlocked(t *testing.T) {
	dep1 := &orchestrator.Task{ID: "dep1", Status: orchestrator.TaskStatusDone}
	dep2 := &orchestrator.Task{ID: "dep2", Status: orchestrator.TaskStatusDone}
	task := &orchestrator.Task{
		ID:        "t1",
		Status:    orchestrator.TaskStatusPending,
		DependsOn: []string{"dep1", "dep2"},
	}
	if orchestrator.ComputeTaskBlocked(task, makeTaskMap(dep1, dep2, task)) {
		t.Error("pending task with all done deps should not be blocked")
	}
}

// --- ResolvePayloadValue: artifact.children.* virtual 評価 ---

func TestResolvePayloadValue_ChildrenAllDone_ZeroChildren_Falsy(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", TotalChildCount: 0, DoneChildCount: 0}
	v, err := orchestrator.ResolvePayloadValue(dep, "artifact.children.all_done")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	b, ok := v.(bool)
	if !ok || b {
		t.Fatalf("want false (no children), got %v", v)
	}
}

func TestResolvePayloadValue_ChildrenAllDone_AllDone_Truthy(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", TotalChildCount: 3, DoneChildCount: 3}
	v, err := orchestrator.ResolvePayloadValue(dep, "artifact.children.all_done")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	b, ok := v.(bool)
	if !ok || !b {
		t.Fatalf("want true (all done), got %v", v)
	}
}

func TestResolvePayloadValue_ChildrenAllDone_PartialDone_Falsy(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", TotalChildCount: 3, DoneChildCount: 2}
	v, err := orchestrator.ResolvePayloadValue(dep, "artifact.children.all_done")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	b, ok := v.(bool)
	if !ok || b {
		t.Fatalf("want false (partial done), got %v", v)
	}
}

func TestResolvePayloadValue_ChildrenAllDone_AllAborted_Falsy(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", TotalChildCount: 3, AbortedChildCount: 3, DoneChildCount: 0}
	v, err := orchestrator.ResolvePayloadValue(dep, "artifact.children.all_done")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	b, ok := v.(bool)
	if !ok || b {
		t.Fatalf("want false (all aborted, not done), got %v", v)
	}
}

func TestResolvePayloadValue_ChildrenAllResolved_DoneAndAborted_Truthy(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", TotalChildCount: 3, DoneChildCount: 2, AbortedChildCount: 1}
	v, err := orchestrator.ResolvePayloadValue(dep, "artifact.children.all_resolved")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	b, ok := v.(bool)
	if !ok || !b {
		t.Fatalf("want true (done+aborted==total), got %v", v)
	}
}

func TestResolvePayloadValue_ChildrenAllResolved_AllAborted_Truthy(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", TotalChildCount: 3, AbortedChildCount: 3, DoneChildCount: 0}
	v, err := orchestrator.ResolvePayloadValue(dep, "artifact.children.all_resolved")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	b, ok := v.(bool)
	if !ok || !b {
		t.Fatalf("want true (all aborted counts as resolved), got %v", v)
	}
}

func TestResolvePayloadValue_ChildrenAllResolved_PartialOpen_Falsy(t *testing.T) {
	dep := &orchestrator.Task{ID: "dep", TotalChildCount: 3, DoneChildCount: 1, AbortedChildCount: 1}
	v, err := orchestrator.ResolvePayloadValue(dep, "artifact.children.all_resolved")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	b, ok := v.(bool)
	if !ok || b {
		t.Fatalf("want false (one child still open), got %v", v)
	}
}

func TestResolvePayloadValue_NonVirtualKey_UsesPayload(t *testing.T) {
	payload := json.RawMessage(`{"artifact": {"pr": {"merged": true}}}`)
	dep := &orchestrator.Task{ID: "dep", Payload: payload}
	v, err := orchestrator.ResolvePayloadValue(dep, "artifact.pr.merged")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	b, ok := v.(bool)
	if !ok || !b {
		t.Fatalf("want true from payload, got %v", v)
	}
}

func TestComputeTaskBlocked_ChildrenAllDone_VirtualKey_AllDone_NotBlocked(t *testing.T) {
	parent := &orchestrator.Task{
		ID:              "parent",
		Status:          orchestrator.TaskStatusDone,
		TotalChildCount: 2,
		DoneChildCount:  2,
	}
	task := &orchestrator.Task{
		ID:               "next",
		Status:           orchestrator.TaskStatusPending,
		DependsOn:        []string{"parent"},
		DependsOnPayload: "artifact.children.all_done",
	}
	if orchestrator.ComputeTaskBlocked(task, makeTaskMap(parent, task)) {
		t.Error("should not be blocked: all children done")
	}
}

func TestComputeTaskBlocked_ChildrenAllDone_VirtualKey_PartialDone_Blocked(t *testing.T) {
	parent := &orchestrator.Task{
		ID:              "parent",
		Status:          orchestrator.TaskStatusDone,
		TotalChildCount: 2,
		DoneChildCount:  1,
	}
	task := &orchestrator.Task{
		ID:               "next",
		Status:           orchestrator.TaskStatusPending,
		DependsOn:        []string{"parent"},
		DependsOnPayload: "artifact.children.all_done",
	}
	if !orchestrator.ComputeTaskBlocked(task, makeTaskMap(parent, task)) {
		t.Error("should be blocked: not all children done")
	}
}

func TestListTasks_Blocked_ComputedCorrectly(t *testing.T) {
	d := createTestProject(t)

	depDone := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Done dep",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, depDone); err != nil {
		t.Fatalf("create depDone: %v", err)
	}

	depPending := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Pending dep",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, depPending); err != nil {
		t.Fatalf("create depPending: %v", err)
	}

	// blockedTask: pending dep がいるので Blocked=true になるはず
	blockedTask := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Blocked task",
		Behavior:  "dev",
		DependsOn: []string{depPending.ID},
	}
	if err := orchestrator.CreateTask(d.Conn, blockedTask); err != nil {
		t.Fatalf("create blockedTask: %v", err)
	}

	// readyTask: done dep しかいないので Blocked=false になるはず
	readyTask := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Ready task",
		Behavior:  "dev",
		DependsOn: []string{depDone.ID},
	}
	if err := orchestrator.CreateTask(d.Conn, readyTask); err != nil {
		t.Fatalf("create readyTask: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}

	taskByID := make(map[string]*orchestrator.Task)
	for _, tsk := range tasks {
		taskByID[tsk.ID] = tsk
	}

	if bt, ok := taskByID[blockedTask.ID]; !ok || !bt.Blocked {
		t.Errorf("blockedTask: want Blocked=true, got Blocked=%v", taskByID[blockedTask.ID].Blocked)
	}
	if rt, ok := taskByID[readyTask.ID]; !ok || rt.Blocked {
		t.Errorf("readyTask: want Blocked=false, got Blocked=%v", taskByID[readyTask.ID].Blocked)
	}
}
