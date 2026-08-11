package api

import (
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// cross-project-issue-triage Phase 1 PR-1: CreateTaskRequest.InitialStatus.

func newInitialStatusTestService() *TaskAppService {
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "main",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"triage": {},
		},
		DefaultTaskBehavior: "triage",
	}
	return &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}
}

func TestCreateTask_InitialStatus_DefaultsToPending(t *testing.T) {
	svc := newInitialStatusTestService()
	task, err := svc.CreateTask(CreateTaskRequest{ProjectID: "proj-1", Title: "t", Behavior: "triage"})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.Status != orchestrator.TaskStatusPending {
		t.Fatalf("Status = %q, want %q", task.Status, orchestrator.TaskStatusPending)
	}
}

func TestCreateTask_InitialStatus_CapturedAndTriaged(t *testing.T) {
	for _, want := range []orchestrator.TaskStatus{orchestrator.TaskStatusCaptured, orchestrator.TaskStatusTriaged} {
		svc := newInitialStatusTestService()
		task, err := svc.CreateTask(CreateTaskRequest{
			ProjectID:     "proj-1",
			Title:         "t",
			Behavior:      "triage",
			InitialStatus: string(want),
		})
		if err != nil {
			t.Fatalf("InitialStatus=%q: CreateTask() error = %v", want, err)
		}
		if task.Status != want {
			t.Fatalf("InitialStatus=%q: Status = %q, want %q", want, task.Status, want)
		}
	}
}

// InitialStatus の allowlist は KnownTaskStatuses 全部ではなく
// pending/captured/triaged だけ (Opus指摘: 呼び出し側が最初から
// executing/done/parked 等を偽装できてはいけない)。
func TestCreateTask_InitialStatus_RejectsDisallowedValues(t *testing.T) {
	for _, bad := range []string{"executing", "done", "aborted", "parked", "ready", "dropped", "garbage"} {
		svc := newInitialStatusTestService()
		_, err := svc.CreateTask(CreateTaskRequest{
			ProjectID:     "proj-1",
			Title:         "t",
			Behavior:      "triage",
			InitialStatus: bad,
		})
		if err == nil {
			t.Fatalf("InitialStatus=%q: expected error, got nil", bad)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusBadRequest {
			t.Fatalf("InitialStatus=%q: expected StatusBadRequest, got %v", bad, err)
		}
	}
}

// auto_start:true は task が pending に留まることを前提にしている
// (TaskStatusPending チェック後に start action を投げる) ので、非pending な
// initial_status との組み合わせは create 時点で明示的に拒否する
// (Opus指摘#10: 黙って無視されると気づけない)。
func TestCreateTask_InitialStatus_RejectsAutoStartCombination(t *testing.T) {
	svc := newInitialStatusTestService()
	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "t",
		Behavior:      "triage",
		InitialStatus: "captured",
		AutoStart:     true,
	})
	if err == nil {
		t.Fatal("expected error combining auto_start with a non-pending initial_status, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected StatusBadRequest, got %v", err)
	}
}

func TestCreateTask_InitialStatus_AutoStartWithPendingIsFine(t *testing.T) {
	svc := newInitialStatusTestService()
	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "t",
		Behavior:      "triage",
		InitialStatus: "pending",
		AutoStart:     true,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil (Workflow is nil so auto_start is a silent no-op, not an error)", err)
	}
}

// Opus指摘#13: meta project の project.yaml に triage 用 behavior/default が
// 無い場合の挙動を固定する。今の CreateTask は Behavior 未指定・
// DefaultTaskBehavior 未設定なら ResolveBehavior が 400 を返す — captured
// task 作成にも同じ制約がそのままかかることを確認する。
func TestCreateTask_InitialStatus_FailsWithoutResolvableBehavior(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{}}, // no TaskBehaviors, no DefaultTaskBehavior
	}
	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "t",
		InitialStatus: "captured",
	})
	if err == nil {
		t.Fatal("expected error when no behavior can be resolved, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected StatusBadRequest (from ResolveBehavior), got %v", err)
	}
}
