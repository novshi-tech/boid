package api

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// multiMetaStore は複数プロジェクトの ProjectMeta を保持するテスト用スタブ。
type multiMetaStore struct {
	metas map[string]*orchestrator.ProjectMeta
}

func (s multiMetaStore) Get(id string) (*orchestrator.ProjectMeta, bool) {
	m, ok := s.metas[id]
	return m, ok
}

// TestTriggerDependentTasks_CrossProject_AutoStartsDependentInOtherProject は
// project_a の task が done になったとき、project_b の依存 task が
// triggerDependentTasks によって auto-start されることを検証する。
func TestTriggerDependentTasks_CrossProject_AutoStartsDependentInOtherProject(t *testing.T) {
	behavior := map[string]orchestrator.TaskBehavior{
		"dev": {},
	}
	metaA := &orchestrator.ProjectMeta{TaskBehaviors: behavior}
	metaB := &orchestrator.ProjectMeta{TaskBehaviors: behavior}

	// project_a の taskDone は done 状態（trigger 元）
	taskDone := &orchestrator.Task{
		ID:        "task-project-a-done",
		ProjectID: "project-a",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
	}
	// project_b の taskPending は project_a の taskDone に依存する pending タスク
	taskPending := &orchestrator.Task{
		ID:        "task-project-b-pending",
		ProjectID: "project-b",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "dev",
		AutoStart: true,
		Payload:   []byte(`{}`),
		DependsOn: []string{taskDone.ID},
	}

	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		taskDone.ID:    taskDone,
		taskPending.ID: taskPending,
	}}
	txStore := &recordingTxStore{task: taskPending}

	svc := &TaskWorkflowService{
		Tasks: store,
		Tx:    recordingTransactor{store: txStore},
		Meta: multiMetaStore{metas: map[string]*orchestrator.ProjectMeta{
			"project-a": metaA,
			"project-b": metaB,
		}},
		// Coordinator は nil にしてバックグラウンドディスパッチを抑制する
	}

	svc.triggerDependentTasks(context.Background(), taskDone.ID)

	// taskPending が start アクションで executing に遷移していることを確認する
	if txStore.updatedTask == nil {
		t.Fatal("expected UpdateTask to be called for cross-project dependent, but it was not")
	}
	if txStore.updatedTask.ID != taskPending.ID {
		t.Fatalf("updated task ID = %q, want %q", txStore.updatedTask.ID, taskPending.ID)
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("updated task status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusExecuting)
	}
}

// TestTriggerDependentTasks_CrossProject_DepNotSatisfied_StaysPending は
// cross-project 依存のうち別の依存がまだ完了していない場合に
// auto-start されない（pending のまま）ことを検証する。
func TestTriggerDependentTasks_CrossProject_DepNotSatisfied_StaysPending(t *testing.T) {
	behavior := map[string]orchestrator.TaskBehavior{
		"dev": {},
	}
	metaA := &orchestrator.ProjectMeta{TaskBehaviors: behavior}
	metaB := &orchestrator.ProjectMeta{TaskBehaviors: behavior}

	// taskDoneA は done（project_a）
	taskDoneA := &orchestrator.Task{
		ID:        "task-a-done",
		ProjectID: "project-a",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
	}
	// taskPendingC はまだ pending（project_a）
	taskPendingC := &orchestrator.Task{
		ID:        "task-c-pending",
		ProjectID: "project-a",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "dev",
	}
	// taskB は project_b に属し、taskDoneA と taskPendingC の両方に依存する。
	// AutoStart=true にして「依存未充足」が唯一の skip 理由であることを明示する。
	taskB := &orchestrator.Task{
		ID:        "task-b",
		ProjectID: "project-b",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "dev",
		AutoStart: true,
		Payload:   []byte(`{}`),
		DependsOn: []string{taskDoneA.ID, taskPendingC.ID},
	}

	store := &multiTaskStore{tasks: map[string]*orchestrator.Task{
		taskDoneA.ID:    taskDoneA,
		taskPendingC.ID: taskPendingC,
		taskB.ID:        taskB,
	}}
	txStore := &recordingTxStore{task: taskB}

	svc := &TaskWorkflowService{
		Tasks: store,
		Tx:    recordingTransactor{store: txStore},
		Meta: multiMetaStore{metas: map[string]*orchestrator.ProjectMeta{
			"project-a": metaA,
			"project-b": metaB,
		}},
	}

	// taskDoneA が done になっても、taskPendingC がまだ pending なので taskB は起動しない
	svc.triggerDependentTasks(context.Background(), taskDoneA.ID)

	if txStore.updatedTask != nil {
		t.Fatalf("expected no UpdateTask call (dep not satisfied), got update for task %q with status %q",
			txStore.updatedTask.ID, txStore.updatedTask.Status)
	}
}
