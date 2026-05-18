package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// setupTriggerProject は one-shot トランジションを持つプロジェクトを作成する。
func setupTriggerProject(t *testing.T, ts *testutil.TestServer, id string) {
	t.Helper()
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid: %v", err)
	}
	yaml := "id: " + id + "\nname: Trigger Test\ntask_behaviors:\n  impl:\n    name: Impl\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project %q: %v", id, err)
	}
}

// waitForStatus は taskID のステータスが want になるまで最大 timeout 待機する。
func waitForStatus(t *testing.T, ts *testutil.TestServer, taskID string, want orchestrator.TaskStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var task orchestrator.Task
		if err := ts.Client.Do("GET", "/api/tasks/"+taskID, nil, &task); err != nil {
			t.Fatalf("get task %s: %v", taskID, err)
		}
		if task.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	if task.Status != want {
		t.Fatalf("task %s: status = %q, want %q (timeout %v)", taskID, task.Status, want, timeout)
	}
}

// createImplTask creates a task with auto_start=false (default).
// Use createImplTaskAutoStart when the test expects the task to auto-start
// on dependency resolution.
func createImplTask(t *testing.T, ts *testutil.TestServer, projectID, title, ref, parentID string, dependsOn []string, dependsOnPayload string) orchestrator.Task {
	return createImplTaskWithAutoStart(t, ts, projectID, title, ref, parentID, dependsOn, dependsOnPayload, false)
}

func createImplTaskAutoStart(t *testing.T, ts *testutil.TestServer, projectID, title, ref, parentID string, dependsOn []string, dependsOnPayload string) orchestrator.Task {
	return createImplTaskWithAutoStart(t, ts, projectID, title, ref, parentID, dependsOn, dependsOnPayload, true)
}

func createImplTaskWithAutoStart(t *testing.T, ts *testutil.TestServer, projectID, title, ref, parentID string, dependsOn []string, dependsOnPayload string, autoStart bool) orchestrator.Task {
	t.Helper()
	req := map[string]any{
		"project_id": projectID,
		"title":      title,
		"behavior":   "impl",
		"auto_start": autoStart,
	}
	if ref != "" {
		req["ref"] = ref
	}
	if parentID != "" {
		req["parent_id"] = parentID
	}
	if len(dependsOn) > 0 {
		req["depends_on"] = dependsOn
	}
	if dependsOnPayload != "" {
		req["depends_on_payload"] = dependsOnPayload
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", req, &task); err != nil {
		t.Fatalf("create task %q: %v", title, err)
	}
	return task
}

func applyImplAction(t *testing.T, ts *testutil.TestServer, taskID, actionType string) {
	t.Helper()
	req := map[string]any{"type": actionType}
	if err := ts.Client.Do("POST", "/api/tasks/"+taskID+"/actions", req, nil); err != nil {
		t.Fatalf("apply action %q to %s: %v", actionType, taskID, err)
	}
}

// TestAutoTrigger_TaskDone_SingleDependent_AutoStarts:
// タスク A が done → タスク B（A のみ依存）が自動 start
func TestAutoTrigger_TaskDone_SingleDependent_AutoStarts(t *testing.T) {
	ts := testutil.NewTestServer(t)
	setupTriggerProject(t, ts, "proj-trigger-1")

	taskA := createImplTask(t, ts, "proj-trigger-1", "Task A", "a", "p1", nil, "")
	taskB := createImplTaskAutoStart(t, ts, "proj-trigger-1", "Task B (depends on A)", "b", "p1", []string{"a"}, "")

	// B は pending のはず
	if taskB.Status != orchestrator.TaskStatusPending {
		t.Fatalf("taskB initial status = %q, want pending", taskB.Status)
	}

	// A を start → done（one-shot: manual done）
	applyImplAction(t, ts, taskA.ID, "start")
	applyImplAction(t, ts, taskA.ID, "done")

	// B が自動的に executing になるまで待つ
	waitForStatus(t, ts, taskB.ID, orchestrator.TaskStatusExecuting, 2*time.Second)
}

// TestAutoTrigger_TaskDone_PartialDeps_StaysPending:
// タスク A done → タスク B（A と C に依存）は C が未 done なので pending 維持
func TestAutoTrigger_TaskDone_PartialDeps_StaysPending(t *testing.T) {
	ts := testutil.NewTestServer(t)
	setupTriggerProject(t, ts, "proj-trigger-2")

	taskA := createImplTask(t, ts, "proj-trigger-2", "Task A", "a", "p2", nil, "")
	taskC := createImplTask(t, ts, "proj-trigger-2", "Task C", "c", "p2", nil, "")
	taskB := createImplTaskAutoStart(t, ts, "proj-trigger-2", "Task B (depends on A and C)", "b", "p2",
		[]string{"a", "c"}, "")

	// A を done にする
	applyImplAction(t, ts, taskA.ID, "start")
	applyImplAction(t, ts, taskA.ID, "done")

	// 少し待って B がまだ pending であることを確認
	time.Sleep(100 * time.Millisecond)
	var b orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+taskB.ID, nil, &b); err != nil {
		t.Fatalf("get taskB: %v", err)
	}
	if b.Status != orchestrator.TaskStatusPending {
		t.Fatalf("taskB status = %q, want pending (C not done yet)", b.Status)
	}

	// C も done にする → B が自動 start されるはず
	applyImplAction(t, ts, taskC.ID, "start")
	applyImplAction(t, ts, taskC.ID, "done")

	waitForStatus(t, ts, taskB.ID, orchestrator.TaskStatusExecuting, 2*time.Second)
}

// TestAutoTrigger_PayloadUpdate_WithPayloadCondition_TriggersDependents:
// payload 更新で DependsOnPayload 条件充足 → 後続タスクが自動 start
func TestAutoTrigger_PayloadUpdate_WithPayloadCondition_TriggersDependents(t *testing.T) {
	ts := testutil.NewTestServer(t)
	setupTriggerProject(t, ts, "proj-trigger-3")

	// Task A: 依存元（done に遷移しておく）
	taskA := createImplTask(t, ts, "proj-trigger-3", "Task A", "a", "p3", nil, "")
	applyImplAction(t, ts, taskA.ID, "start")
	applyImplAction(t, ts, taskA.ID, "done")

	// Task B: A が done かつ A.payload["pr_merged"] が truthy のときだけ start できる
	taskB := createImplTaskAutoStart(t, ts, "proj-trigger-3", "Task B (payload dep)", "b", "p3", []string{"a"}, "pr_merged")

	// A の payload に pr_merged=false をセット → B は pending 維持
	patchFalse := map[string]any{
		"payload": json.RawMessage(`{"pr_merged": false}`),
	}
	if err := ts.Client.Do("PATCH", "/api/tasks/"+taskA.ID, patchFalse, nil); err != nil {
		t.Fatalf("patch taskA (pr_merged=false): %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	var b orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+taskB.ID, nil, &b); err != nil {
		t.Fatalf("get taskB: %v", err)
	}
	if b.Status != orchestrator.TaskStatusPending {
		t.Fatalf("taskB status = %q, want pending (pr_merged=false)", b.Status)
	}

	// A の payload に pr_merged=true をセット → B が自動 start されるはず
	patchTrue := map[string]any{
		"payload": json.RawMessage(`{"pr_merged": true}`),
	}
	if err := ts.Client.Do("PATCH", "/api/tasks/"+taskA.ID, patchTrue, nil); err != nil {
		t.Fatalf("patch taskA (pr_merged=true): %v", err)
	}

	waitForStatus(t, ts, taskB.ID, orchestrator.TaskStatusExecuting, 2*time.Second)
}

// TestAutoTrigger_ChildrenAllDone_Phase2AutoStarts:
// Phase1 (plan) が done に遷移後、子タスクが全て done になると
// `depends_on_payload: "artifact.children.all_done"` の Phase2 が自動 start する。
func TestAutoTrigger_ChildrenAllDone_Phase2AutoStarts(t *testing.T) {
	ts := testutil.NewTestServer(t)
	setupTriggerProject(t, ts, "proj-children-done")

	// Phase1 タスクを作成して done にする（plan タスクの代替として impl で代用）
	phase1 := createImplTask(t, ts, "proj-children-done", "Phase1", "phase1", "", nil, "")
	applyImplAction(t, ts, phase1.ID, "start")
	applyImplAction(t, ts, phase1.ID, "done")

	// Phase1 の子タスクを 2 つ作成
	child1 := createImplTask(t, ts, "proj-children-done", "Child 1", "c1", phase1.ID, nil, "")
	child2 := createImplTask(t, ts, "proj-children-done", "Child 2", "c2", phase1.ID, nil, "")

	// Phase2: phase1 が done かつ artifact.children.all_done が true になると start
	phase2 := createImplTaskAutoStart(t, ts, "proj-children-done", "Phase2", "phase2", "", []string{phase1.ID}, "artifact.children.all_done")

	// Phase2 はまだ pending のはず
	if phase2.Status != orchestrator.TaskStatusPending {
		t.Fatalf("phase2 initial status = %q, want pending", phase2.Status)
	}

	// child1 が done → 子全員未完了なので phase2 は pending のまま
	applyImplAction(t, ts, child1.ID, "start")
	applyImplAction(t, ts, child1.ID, "done")
	time.Sleep(100 * time.Millisecond)
	var p2 orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+phase2.ID, nil, &p2); err != nil {
		t.Fatalf("get phase2: %v", err)
	}
	if p2.Status != orchestrator.TaskStatusPending {
		t.Fatalf("phase2 status = %q, want pending (child2 not done yet)", p2.Status)
	}

	// child2 も done → phase2 が自動 start されるはず
	applyImplAction(t, ts, child2.ID, "start")
	applyImplAction(t, ts, child2.ID, "done")
	waitForStatus(t, ts, phase2.ID, orchestrator.TaskStatusExecuting, 2*time.Second)
}

// TestAutoTrigger_ChildrenAllDone_AbortedChild_StaysPending:
// 子が一部 aborted → all_done は立たず phase2 は pending のまま。
func TestAutoTrigger_ChildrenAllDone_AbortedChild_StaysPending(t *testing.T) {
	ts := testutil.NewTestServer(t)
	setupTriggerProject(t, ts, "proj-children-aborted")

	phase1 := createImplTask(t, ts, "proj-children-aborted", "Phase1", "phase1", "", nil, "")
	applyImplAction(t, ts, phase1.ID, "start")
	applyImplAction(t, ts, phase1.ID, "done")

	child1 := createImplTask(t, ts, "proj-children-aborted", "Child 1", "c1", phase1.ID, nil, "")
	child2 := createImplTask(t, ts, "proj-children-aborted", "Child 2", "c2", phase1.ID, nil, "")

	phase2 := createImplTaskAutoStart(t, ts, "proj-children-aborted", "Phase2 all_done", "phase2", "", []string{phase1.ID}, "artifact.children.all_done")

	// child1 done, child2 aborted
	applyImplAction(t, ts, child1.ID, "start")
	applyImplAction(t, ts, child1.ID, "done")
	applyImplAction(t, ts, child2.ID, "abort")

	time.Sleep(200 * time.Millisecond)
	var p2 orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+phase2.ID, nil, &p2); err != nil {
		t.Fatalf("get phase2: %v", err)
	}
	if p2.Status != orchestrator.TaskStatusPending {
		t.Fatalf("phase2 status = %q, want pending (aborted child → all_done false)", p2.Status)
	}
}

// TestAutoTrigger_ChildrenAllResolved_AbortedChild_AutoStarts:
// 子が done + aborted で全員解決 → all_resolved が立ち phase2 が自動 start。
func TestAutoTrigger_ChildrenAllResolved_AbortedChild_AutoStarts(t *testing.T) {
	ts := testutil.NewTestServer(t)
	setupTriggerProject(t, ts, "proj-children-resolved")

	phase1 := createImplTask(t, ts, "proj-children-resolved", "Phase1", "phase1", "", nil, "")
	applyImplAction(t, ts, phase1.ID, "start")
	applyImplAction(t, ts, phase1.ID, "done")

	child1 := createImplTask(t, ts, "proj-children-resolved", "Child 1", "c1", phase1.ID, nil, "")
	child2 := createImplTask(t, ts, "proj-children-resolved", "Child 2", "c2", phase1.ID, nil, "")

	phase2 := createImplTaskAutoStart(t, ts, "proj-children-resolved", "Phase2 all_resolved", "phase2r", "", []string{phase1.ID}, "artifact.children.all_resolved")

	applyImplAction(t, ts, child1.ID, "start")
	applyImplAction(t, ts, child1.ID, "done")
	applyImplAction(t, ts, child2.ID, "abort")

	waitForStatus(t, ts, phase2.ID, orchestrator.TaskStatusExecuting, 2*time.Second)
}

// TestAutoTrigger_AutoStartFalse_Dependent_StaysPending:
// 依存条件が満たされても、auto_start=false の依存タスクは start しない。
// ユーザがステップごとに確認しながら進めたいケースの検証。
func TestAutoTrigger_AutoStartFalse_Dependent_StaysPending(t *testing.T) {
	ts := testutil.NewTestServer(t)
	setupTriggerProject(t, ts, "proj-trigger-no-auto")

	taskA := createImplTask(t, ts, "proj-trigger-no-auto", "Task A", "a", "pna", nil, "")
	// Task B は auto_start=false (createImplTask のデフォルト) → A が done になっても
	// 自動 start されず pending のまま残る。
	taskB := createImplTask(t, ts, "proj-trigger-no-auto", "Task B (manual)", "b", "pna",
		[]string{"a"}, "")

	applyImplAction(t, ts, taskA.ID, "start")
	applyImplAction(t, ts, taskA.ID, "done")

	// 少し待って B がまだ pending であることを確認
	time.Sleep(200 * time.Millisecond)
	var b orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+taskB.ID, nil, &b); err != nil {
		t.Fatalf("get taskB: %v", err)
	}
	if b.Status != orchestrator.TaskStatusPending {
		t.Fatalf("taskB status = %q, want pending (auto_start=false)", b.Status)
	}

	// 手動で start すれば通る
	applyImplAction(t, ts, taskB.ID, "start")
	waitForStatus(t, ts, taskB.ID, orchestrator.TaskStatusExecuting, 2*time.Second)
}

// TestAutoTrigger_CircularDep_CreateError:
// 循環依存のタスク作成はエラー（API レベル）
func TestAutoTrigger_CircularDep_CreateError(t *testing.T) {
	ts := testutil.NewTestServer(t)
	setupTriggerProject(t, ts, "proj-trigger-cycle")

	taskA := createImplTask(t, ts, "proj-trigger-cycle", "Task A", "a", "pcyc", nil, "")
	_ = createImplTask(t, ts, "proj-trigger-cycle", "Task B", "b", "pcyc", []string{"a"}, "")
	_ = createImplTask(t, ts, "proj-trigger-cycle", "Task C", "c", "pcyc", []string{"b"}, "")

	// taskA の ID で taskC に依存するタスクを作ろうとする → 循環依存エラー
	cycleReq := map[string]any{
		"id":         taskA.ID,
		"project_id": "proj-trigger-cycle",
		"title":      "Cyclic A",
		"behavior":   "impl",
		"parent_id":  "pcyc",
		"depends_on": []string{"c"},
	}
	err := ts.Client.Do("POST", "/api/tasks", cycleReq, nil)
	if err == nil {
		t.Fatal("POST /api/tasks: error = nil, want error for circular dependency")
	}
}
