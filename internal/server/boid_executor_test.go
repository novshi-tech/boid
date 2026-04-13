package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type capturingTaskStore struct {
	created           []*orchestrator.Task
	updated           []*orchestrator.Task
	findByRemoteFunc  func(remoteID, datasourceID string) (*orchestrator.Task, error)
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
	if s.findByRemoteFunc != nil {
		return s.findByRemoteFunc(remoteID, datasourceID)
	}
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
			"dev": {},
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
// 検証し、payload を top-level shallow merge する。
// 案 B: artifact.<handler-role> が別 top-level キーになるため、
// artifact.run-agent と artifact.auto-merge は別キーとして shallow merge で保持される。
func TestBoidBuiltinExecutor_TaskUpdate_EnforcesWorkspaceScope(t *testing.T) {
	store := &capturingTaskStore{
		created: []*orchestrator.Task{
			// target-1: 既存 payload に instructions と artifact.run-agent を持つ
			{ID: "target-1", ProjectID: "proj-1", Payload: []byte(`{"instructions":{"main":{"consumer":"claude-code"}},"artifact.run-agent":{"commit":"abc","branch":"boid/target"}}`)},
			{ID: "peer-1", ProjectID: "proj-2", Payload: []byte(`{}`)},
			{ID: "foreign-1", ProjectID: "proj-3", Payload: []byte(`{}`)},
		},
	}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
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
	// shallow merge のため artifact.run-agent (別 top-level キー) は保持され、
	// artifact.auto-merge が追記される。
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:      sandbox.BoidOpTaskUpdate,
		TaskID:  "target-1",
		Payload: []byte(`{"artifact.auto-merge":{"pr":{"merged":true,"number":42}}}`),
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
	// 既存の artifact.run-agent は別 top-level キーなので shallow merge で保持される
	if !strings.Contains(got, `"artifact.run-agent"`) {
		t.Errorf("merged payload = %s, want artifact.run-agent key preserved", got)
	}
	if !strings.Contains(got, `"commit":"abc"`) {
		t.Errorf("merged payload = %s, want existing commit preserved", got)
	}
	// 新規 artifact.auto-merge が追加される
	if !strings.Contains(got, `"artifact.auto-merge"`) {
		t.Errorf("merged payload = %s, want artifact.auto-merge key added", got)
	}
	if !strings.Contains(got, `"merged":true`) || !strings.Contains(got, `"number":42`) {
		t.Errorf("merged payload = %s, want new pr fields", got)
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
			"dev": {},
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
		DependsOnPayload: "artifact.auto-merge.pr.merged",
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
	if got.DependsOnPayload != "artifact.auto-merge.pr.merged" {
		t.Errorf("depends_on_payload = %q, want artifact.auto-merge.pr.merged", got.DependsOnPayload)
	}
	if want := []string{"id-a", "id-b"}; len(got.DependsOn) != len(want) || got.DependsOn[0] != want[0] || got.DependsOn[1] != want[1] {
		t.Errorf("depends_on = %v, want %v (resolved IDs)", got.DependsOn, want)
	}
}

// --- task import executor tests ---

func newImportExecutor(t *testing.T) (*boidBuiltinExecutor, *capturingTaskStore) {
	t.Helper()
	store := &capturingTaskStore{}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}}
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: store, Meta: meta},
	}
	return exec, store
}

func TestBoidBuiltinExecutor_TaskImport_HappyPath(t *testing.T) {
	exec, store := newImportExecutor(t)
	ctx := sandbox.TokenContext{
		ProjectID:         "proj-1",
		AllowedProjectIDs: []string{"proj-1"},
	}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskImport,
		ImportTasks: []json.RawMessage{
			json.RawMessage(`{"project_id":"proj-1","title":"t1","behavior":"dev","remote_id":"r1","datasource_id":"ds1"}`),
			json.RawMessage(`{"project_id":"proj-1","title":"t2","behavior":"dev","remote_id":"r2","datasource_id":"ds1"}`),
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "Created: 2, Skipped: 0, Errors: 0\n" {
		t.Errorf("stdout = %q, want %q", resp.Stdout, "Created: 2, Skipped: 0, Errors: 0\n")
	}
	if len(store.created) != 2 {
		t.Fatalf("created tasks = %d, want 2", len(store.created))
	}
}

func TestBoidBuiltinExecutor_TaskImport_DatasourceOverride(t *testing.T) {
	exec, store := newImportExecutor(t)
	ctx := sandbox.TokenContext{
		ProjectID:         "proj-1",
		AllowedProjectIDs: []string{"proj-1"},
	}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:                      sandbox.BoidOpTaskImport,
		ImportDatasourceOverride: "ds-override",
		ImportTasks: []json.RawMessage{
			json.RawMessage(`{"project_id":"proj-1","title":"t1","behavior":"dev","remote_id":"r1","datasource_id":"ds-original"}`),
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(store.created))
	}
	if store.created[0].DataSourceID != "ds-override" {
		t.Errorf("DataSourceID = %q, want ds-override", store.created[0].DataSourceID)
	}
}

func TestBoidBuiltinExecutor_TaskImport_ProjectOverride(t *testing.T) {
	exec, store := newImportExecutor(t)
	ctx := sandbox.TokenContext{
		ProjectID:         "proj-1",
		AllowedProjectIDs: []string{"proj-1"},
	}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:                   sandbox.BoidOpTaskImport,
		ImportProjectOverride: "proj-1",
		ImportTasks: []json.RawMessage{
			json.RawMessage(`{"title":"t1","behavior":"dev","remote_id":"r1","datasource_id":"ds1"}`), // project_id 未指定
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(store.created))
	}
	if store.created[0].ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want proj-1", store.created[0].ProjectID)
	}
}

func TestBoidBuiltinExecutor_TaskImport_Dedup(t *testing.T) {
	exec, store := newImportExecutor(t)
	// remote_id=r1 が既存として返される
	store.findByRemoteFunc = func(remoteID, datasourceID string) (*orchestrator.Task, error) {
		if remoteID == "r1" {
			return &orchestrator.Task{ID: "existing-1", ProjectID: "proj-1"}, nil
		}
		return nil, nil
	}
	ctx := sandbox.TokenContext{
		ProjectID:         "proj-1",
		AllowedProjectIDs: []string{"proj-1"},
	}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskImport,
		ImportTasks: []json.RawMessage{
			json.RawMessage(`{"project_id":"proj-1","title":"t1","behavior":"dev","remote_id":"r1","datasource_id":"ds1"}`), // dedup
			json.RawMessage(`{"project_id":"proj-1","title":"t2","behavior":"dev","remote_id":"r2","datasource_id":"ds1"}`), // new
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "Created: 1, Skipped: 1, Errors: 0\n" {
		t.Errorf("stdout = %q, want %q", resp.Stdout, "Created: 1, Skipped: 1, Errors: 0\n")
	}
}

func TestBoidBuiltinExecutor_TaskImport_PartialError(t *testing.T) {
	exec, _ := newImportExecutor(t)
	ctx := sandbox.TokenContext{
		ProjectID:         "proj-1",
		AllowedProjectIDs: []string{"proj-1"},
	}

	// 1件目: behavior 欠落 (remote_id/datasource_id あり) → CreateTask でエラー
	// 2件目: 正常
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskImport,
		ImportTasks: []json.RawMessage{
			json.RawMessage(`{"project_id":"proj-1","title":"t1","remote_id":"r1","datasource_id":"ds1"}`),
			json.RawMessage(`{"project_id":"proj-1","title":"t2","behavior":"dev","remote_id":"r2","datasource_id":"ds1"}`),
		},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d (partial errors should not set exit_code=1), stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "Created: 1, Skipped: 0, Errors: 1\n" {
		t.Errorf("stdout = %q, want %q", resp.Stdout, "Created: 1, Skipped: 0, Errors: 1\n")
	}
	if !strings.Contains(resp.Stderr, "error line 1") {
		t.Errorf("stderr = %q, want 'error line 1'", resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_TaskImport_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{tasks: nil}
	ctx := sandbox.TokenContext{ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskImport,
		ImportTasks: []json.RawMessage{
			json.RawMessage(`{"title":"t"}`),
		},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "task import unavailable") {
		t.Errorf("stderr = %q, want 'task import unavailable'", resp.Stderr)
	}
}
