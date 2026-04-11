package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type capturingTaskStore struct {
	created []*orchestrator.Task
	updated []*orchestrator.Task
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
	for _, t := range s.created {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("task not found: %s", id)
}

func (s *capturingTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}

func (s *capturingTaskStore) UpdateTask(task *orchestrator.Task) error {
	s.updated = append(s.updated, task)
	for i, t := range s.created {
		if t.ID == task.ID {
			s.created[i] = task
			return nil
		}
	}
	return nil
}
func (s *capturingTaskStore) DeleteTask(id string) error { return nil }
func (s *capturingTaskStore) FindTaskByRemote(remoteID, datasourceID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *capturingTaskStore) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	for _, t := range s.created {
		if t.Ref == ref && t.ParentID == parentID {
			return t, nil
		}
	}
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

// BoidOpTaskUpdate は対象 task の project_id が現在の workspace に含まれるかを
// 検証し、payload を nested deep merge する (DeepMergePayload の挙動)。
// 既存の artifact.commit を保持したまま artifact.pr を追記するユースケースが
// auto-merge gate の典型的な使い方。
func TestBoidBuiltinExecutor_TaskUpdate_EnforcesWorkspaceScope(t *testing.T) {
	store := &capturingTaskStore{
		created: []*orchestrator.Task{
			// target-1: 既存 payload に instructions と artifact.commit を持つ
			{ID: "target-1", ProjectID: "proj-1", Payload: []byte(`{"instructions":{"main":{"consumer":"claude-code"}},"artifact":{"commit":"abc","branch":"boid/target"}}`)},
			{ID: "peer-1", ProjectID: "proj-2", Payload: []byte(`{}`)},
			{ID: "foreign-1", ProjectID: "proj-3", Payload: []byte(`{}`)},
		},
	}
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

	// 自プロジェクトのタスクを更新できる。
	// deep merge によって既存の artifact.commit/branch は保持され、
	// artifact.pr が追記される。
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:      sandbox.BoidOpTaskUpdate,
		TaskID:  "target-1",
		Payload: []byte(`{"artifact":{"pr":{"merged":true,"number":42}}}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("self project update exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.updated) != 1 {
		t.Fatalf("updated tasks = %d, want 1", len(store.updated))
	}
	got := string(store.updated[0].Payload)
	// 既存の instructions は保持される
	if !strings.Contains(got, `"instructions"`) {
		t.Errorf("merged payload = %s, want instructions preserved", got)
	}
	// 既存の artifact.commit は保持される (deep merge の効果)
	if !strings.Contains(got, `"commit":"abc"`) {
		t.Errorf("merged payload = %s, want existing artifact.commit preserved", got)
	}
	// 既存の artifact.branch も保持される
	if !strings.Contains(got, `"branch":"boid/target"`) {
		t.Errorf("merged payload = %s, want existing artifact.branch preserved", got)
	}
	// 新規 artifact.pr が追加される
	if !strings.Contains(got, `"merged":true`) || !strings.Contains(got, `"number":42`) {
		t.Errorf("merged payload = %s, want new artifact.pr fields", got)
	}

	// workspace 内の peer プロジェクトのタスクも更新できる
	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:      sandbox.BoidOpTaskUpdate,
		TaskID:  "peer-1",
		Payload: []byte(`{"hello":"world"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("peer project update exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.updated) != 2 {
		t.Fatalf("updated tasks = %d, want 2 after peer update", len(store.updated))
	}

	// workspace 外のタスクは更新できない
	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:      sandbox.BoidOpTaskUpdate,
		TaskID:  "foreign-1",
		Payload: []byte(`{"x":1}`),
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("cross-workspace update should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(store.updated) != 2 {
		t.Fatalf("cross-workspace update should not reach task store, updated=%d", len(store.updated))
	}

	// 存在しない TaskID は NotFound
	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:      sandbox.BoidOpTaskUpdate,
		TaskID:  "unknown",
		Payload: []byte(`{"x":1}`),
	})
	if resp.ExitCode != 1 {
		t.Fatalf("unknown task update should fail, got exit=%d", resp.ExitCode)
	}
}

// 空の TaskID はエラー
func TestBoidBuiltinExecutor_TaskUpdate_RequiresTaskID(t *testing.T) {
	store := &capturingTaskStore{}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{}}
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: store, Meta: meta},
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:      sandbox.BoidOpTaskUpdate,
		Payload: []byte(`{"x":1}`),
	})
	if resp.ExitCode != 1 {
		t.Fatalf("expected error, got exit=%d", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "task id") {
		t.Errorf("stderr = %q, want 'task id' error", resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_PropagatesDependencyFields(t *testing.T) {
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
		AllowedProjectIDs: []string{"proj-1"},
	}

	// Pre-populate dependency targets so depends_on resolution succeeds.
	store.created = append(store.created,
		&orchestrator.Task{ID: "id-a", Ref: "task-a", ParentID: "parent-1", ProjectID: "proj-1"},
		&orchestrator.Task{ID: "id-b", Ref: "task-b", ParentID: "parent-1", ProjectID: "proj-1"},
	)

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:               sandbox.BoidOpTaskCreate,
		Title:            "child",
		Behavior:         "dev",
		Description:      "desc",
		Ref:              "task-c",
		ParentID:         "parent-1",
		DependsOn:        []string{"task-a", "task-b"},
		DependsOnPayload: "artifact.pr.merged",
		AutoStart:        false, // disable to avoid Workflow nil
	})
	if resp.ExitCode != 0 {
		t.Fatalf("create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}

	if len(store.created) != 3 {
		t.Fatalf("created tasks = %d, want 3", len(store.created))
	}
	got := store.created[2]
	if got.Ref != "task-c" {
		t.Errorf("ref = %q, want task-c", got.Ref)
	}
	if got.ParentID != "parent-1" {
		t.Errorf("parent_id = %q, want parent-1", got.ParentID)
	}
	if got.DependsOnPayload != "artifact.pr.merged" {
		t.Errorf("depends_on_payload = %q, want artifact.pr.merged", got.DependsOnPayload)
	}
	if want := []string{"id-a", "id-b"}; len(got.DependsOn) != len(want) || got.DependsOn[0] != want[0] || got.DependsOn[1] != want[1] {
		t.Errorf("depends_on = %v, want %v (resolved IDs)", got.DependsOn, want)
	}
}
