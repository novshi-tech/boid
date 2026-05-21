package server

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// recordingLifecycle implements api.JobLifecycle for boid_executor agent stop
// tests. It records SignalJobRuntime calls so the test can assert SIGUSR1 was
// delivered to the right runtime; other methods are recorded only to verify
// they are NOT called from the BoidOpAgentStop path.
type recordingLifecycle struct {
	mu                sync.Mutex
	signaledRuntime   string
	signaledSignal    syscall.Signal
	signaledCount     int
	completedJobID    string
	unregisteredJobID string
	stoppedRuntime    string
}

func (l *recordingLifecycle) CompleteJob(jobID string, _ api.JobCompletion) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.completedJobID = jobID
}

func (l *recordingLifecycle) UnregisterJob(jobID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unregisteredJobID = jobID
}

func (l *recordingLifecycle) CleanupTaskWindow(string) {}

func (l *recordingLifecycle) StopJobRuntime(runtimeID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stoppedRuntime = runtimeID
}

func (l *recordingLifecycle) SignalJobRuntime(runtimeID string, sig syscall.Signal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.signaledRuntime = runtimeID
	l.signaledSignal = sig
	l.signaledCount++
}

func (l *recordingLifecycle) waitSignaled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		ok := l.signaledRuntime != ""
		l.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

type capturingTaskStore struct {
	created           []*orchestrator.Task
	updated           []*orchestrator.Task
	deleted           []string
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
func (s *capturingTaskStore) DeleteTask(id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *capturingTaskStore) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	if s.findByRemoteFunc != nil {
		return s.findByRemoteFunc(remoteID, "")
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
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: json.RawMessage(`{"title":"same workspace","behavior":"dev"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("self project create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 || store.created[0].ProjectID != "proj-1" {
		t.Fatalf("created tasks = %+v, want current project", store.created)
	}

	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		ProjectID:   "proj-2",
		CreatePatch: json.RawMessage(`{"title":"peer workspace","behavior":"dev"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("peer project create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 2 || store.created[1].ProjectID != "proj-2" {
		t.Fatalf("created tasks = %+v, want peer project", store.created)
	}

	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		ProjectID:   "proj-3",
		CreatePatch: json.RawMessage(`{"title":"cross workspace","behavior":"dev"}`),
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
			{ID: "target-1", ProjectID: "proj-1", Payload: []byte(`{"instructions":{"main":{"agent":"claude-code"}},"artifact.run-agent":{"commit":"abc","branch":"boid/target"}}`)},
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
		Op:          sandbox.BoidOpTaskUpdate,
		TaskID:      "target-1",
		UpdatePatch: json.RawMessage(`{"payload":{"artifact.auto-merge":{"pr":{"merged":true,"number":42}}}}`),
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
		Op:          sandbox.BoidOpTaskUpdate,
		TaskID:      "peer-1",
		UpdatePatch: json.RawMessage(`{"payload":{"hello":"world"}}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("peer project update exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.updated) != 2 {
		t.Fatalf("updated tasks = %d, want 2 after peer update", len(store.updated))
	}

	// workspace 外のタスクは更新できない
	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskUpdate,
		TaskID:      "foreign-1",
		UpdatePatch: json.RawMessage(`{"payload":{"x":1}}`),
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("cross-workspace update should be rejected, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(store.updated) != 2 {
		t.Fatalf("cross-workspace update should not reach task store, updated=%d", len(store.updated))
	}

	// 存在しない TaskID は NotFound
	resp = exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskUpdate,
		TaskID:      "unknown",
		UpdatePatch: json.RawMessage(`{"payload":{"x":1}}`),
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
		Op:          sandbox.BoidOpTaskUpdate,
		UpdatePatch: json.RawMessage(`{"payload":{"x":1}}`),
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
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: json.RawMessage(`{"title":"child","behavior":"dev","description":"desc","ref":"task-c","parent_id":"parent-1","depends_on":["task-a","task-b"],"depends_on_payload":"artifact.auto-merge.merged","auto_start":false}`),
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
	if got.DependsOnPayload != "artifact.auto-merge.merged" {
		t.Errorf("depends_on_payload = %q, want artifact.auto-merge.merged", got.DependsOnPayload)
	}
	if want := []string{"id-a", "id-b"}; len(got.DependsOn) != len(want) || got.DependsOn[0] != want[0] || got.DependsOn[1] != want[1] {
		t.Errorf("depends_on = %v, want %v (resolved IDs)", got.DependsOn, want)
	}
}

// TestBoidBuiltinExecutor_TaskCreate_DropsDeprecatedBaseBranch covers Phase
// 2-3. Sandbox-side `boid task create` still forwards the entire YAML map,
// so an old caller might keep emitting `base_branch:`. The API server now
// drops the key at decode time and the resulting Task must reflect the
// project-level value instead of the override.
func TestBoidBuiltinExecutor_TaskCreate_DropsDeprecatedBaseBranch(t *testing.T) {
	store := &capturingTaskStore{}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		BaseBranch: "main",
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

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: json.RawMessage(`{"title":"branch override","behavior":"dev","base_branch":"feature/x","readonly":true,"worktree":false,"branch_prefix":"task/"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(store.created))
	}
	if got := store.created[0].BaseBranch; got != "main" {
		t.Errorf("base_branch = %q, want main (deprecated task-row override is dropped)", got)
	}
}

func TestBoidBuiltinExecutor_TaskCreate_BaseBranchInheritsFromProject(t *testing.T) {
	store := &capturingTaskStore{}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		BaseBranch: "main",
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

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: json.RawMessage(`{"title":"no override","behavior":"dev"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if got := store.created[0].BaseBranch; got != "main" {
		t.Errorf("base_branch = %q, want main (inherited from project)", got)
	}
}

func TestBoidBuiltinExecutor_TaskCreate_DefaultsParentIDFromContext(t *testing.T) {
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
		TaskID:            "parent-task-id",
		AllowedProjectIDs: []string{"proj-1"},
	}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: json.RawMessage(`{"title":"child task","behavior":"dev"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(store.created))
	}
	if got := store.created[0].ParentID; got != "parent-task-id" {
		t.Errorf("parent_id = %q, want parent-task-id (from ctx.TaskID)", got)
	}
}

func TestBoidBuiltinExecutor_TaskCreate_ExplicitParentIDOverridesContext(t *testing.T) {
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
		TaskID:            "ctx-task-id",
		AllowedProjectIDs: []string{"proj-1"},
	}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: json.RawMessage(`{"title":"child task","behavior":"dev","parent_id":"explicit-parent-id"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("create exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(store.created))
	}
	if got := store.created[0].ParentID; got != "explicit-parent-id" {
		t.Errorf("parent_id = %q, want explicit-parent-id (explicit overrides ctx.TaskID)", got)
	}
}

// TestBoidBuiltinExecutor_TaskCreate_BrokerResolvedIDOverridesCreatePatch は
// broker が project 名 ("boid-kits") を UUID ("dad1961a-...") に解決済みの場合に
// CreatePatch.project_id (名前) をそのまま AllowsProject に渡すバグを再現する。
// req.ProjectID (UUID) が AllowedProjectIDs に含まれていれば成功しなければならない。
func TestBoidBuiltinExecutor_TaskCreate_BrokerResolvedIDOverridesCreatePatch(t *testing.T) {
	store := &capturingTaskStore{}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}},
	}}
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: store, Meta: meta},
	}
	const peerUUID = "dad1961a-9ef9-495d-858f-e27e75d9afca"
	ctx := sandbox.TokenContext{
		ProjectID:         "boid-main-uuid",
		WorkspaceID:       "ws-boid",
		AllowedProjectIDs: []string{"boid-main-uuid", peerUUID},
	}

	// req.ProjectID = UUID (broker 解決済み)
	// CreatePatch.project_id = 名前 ("boid-kits")  — broker は上書きしない
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		ProjectID:   peerUUID,
		CreatePatch: json.RawMessage(`{"project_id":"boid-kits","title":"peer task","behavior":"dev"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("peer create with broker-resolved UUID should succeed, exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(store.created) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(store.created))
	}
	if store.created[0].ProjectID != peerUUID {
		t.Errorf("created task ProjectID = %q, want %q (broker-resolved UUID)", store.created[0].ProjectID, peerUUID)
	}
}

// --- agent_stop executor tests ---

// BoidOpAgentStop は SignalJobRuntime(SIGUSR1) を runtime に発するだけで、
// CompleteJob / UnregisterJob / StopJobRuntime には触れない。 これは
// CompleteJob の preemptive call が broker token を失効させて bash EXIT trap
// の `boid job done --output-file payload_patch.json` を invalid token と
// して silently drop する notify --ask race (#417) と同じ理由で、 explicit
// exit 経路でも canonical CompleteJob caller を EXIT trap に温存する設計。
func TestBoidBuiltinExecutor_AgentStop_SignalsRuntimeOnly(t *testing.T) {
	jobs := &stubJobStore{
		jobs: map[string]*api.Job{
			"job-1": {
				ID:        "job-1",
				TaskID:    "task-1",
				ProjectID: "proj-1",
				Status:    api.JobStatusRunning,
				RuntimeID: "rt-1",
			},
		},
	}
	lifecycle := &recordingLifecycle{}
	wf := &api.TaskWorkflowService{Lifecycle: lifecycle}
	exec := &boidBuiltinExecutor{workflow: wf, jobs: jobs}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpAgentStop,
		JobID: "job-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "job-1") {
		t.Errorf("stdout = %q, want job-1", resp.Stdout)
	}

	// StopAgent は go-routine 内で SignalJobRuntime を呼ぶ。
	lifecycle.waitSignaled(t)

	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.signaledRuntime != "rt-1" {
		t.Errorf("signaled runtime = %q, want rt-1", lifecycle.signaledRuntime)
	}
	if lifecycle.signaledSignal != syscall.SIGUSR1 {
		t.Errorf("signaled signal = %v, want SIGUSR1", lifecycle.signaledSignal)
	}
	if lifecycle.signaledCount != 1 {
		t.Errorf("signaled count = %d, want 1 (no SIGKILL follow-up)", lifecycle.signaledCount)
	}
	if lifecycle.completedJobID != "" {
		t.Errorf("CompleteJob lifecycle called with %q, want empty (must defer to EXIT trap)", lifecycle.completedJobID)
	}
	if lifecycle.unregisteredJobID != "" {
		t.Errorf("UnregisterJob called with %q, want empty (broker token must stay valid for the EXIT trap)", lifecycle.unregisteredJobID)
	}
	if lifecycle.stoppedRuntime != "" {
		t.Errorf("StopJobRuntime called with %q, want empty (agent stop must not SIGTERM the whole runtime)", lifecycle.stoppedRuntime)
	}
}

// RuntimeID 空 (host foreground job など) は no-op 成功で返す。 失敗ではなく
// 成功にしておかないと、 agent が誤って agent stop を呼んだ場合に bash が
// 後続の `exit 1` を踏んで EXIT trap が failed CompleteJob を送ってしまう。
func TestBoidBuiltinExecutor_AgentStop_NoRuntimeIsSuccess(t *testing.T) {
	jobs := &stubJobStore{
		jobs: map[string]*api.Job{
			"job-foreground": {
				ID:        "job-foreground",
				TaskID:    "task-1",
				ProjectID: "proj-1",
				Status:    api.JobStatusRunning,
				RuntimeID: "",
			},
		},
	}
	lifecycle := &recordingLifecycle{}
	wf := &api.TaskWorkflowService{Lifecycle: lifecycle}
	exec := &boidBuiltinExecutor{workflow: wf, jobs: jobs}
	ctx := sandbox.TokenContext{JobID: "job-foreground", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpAgentStop,
		JobID: "job-foreground",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s (expected 0 for no-runtime no-op)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "no runtime") {
		t.Errorf("stdout = %q, want 'no runtime' hint", resp.Stdout)
	}
	if lifecycle.signaledCount != 0 {
		t.Errorf("signaled count = %d, want 0 (no runtime → no signal)", lifecycle.signaledCount)
	}
}

func TestBoidBuiltinExecutor_AgentStop_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{workflow: nil, jobs: nil}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpAgentStop,
		JobID: "job-1",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
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
			json.RawMessage(`{"project_id":"proj-1","title":"t1","behavior":"dev","remote_id":"r1"}`),
			json.RawMessage(`{"project_id":"proj-1","title":"t2","behavior":"dev","remote_id":"r2"}`),
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
			json.RawMessage(`{"title":"t1","behavior":"dev","remote_id":"r1"}`), // project_id 未指定
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
			json.RawMessage(`{"project_id":"proj-1","title":"t1","behavior":"dev","remote_id":"r1"}`), // dedup
			json.RawMessage(`{"project_id":"proj-1","title":"t2","behavior":"dev","remote_id":"r2"}`), // new
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

	// 1件目: behavior 欠落 → CreateTask でエラー
	// 2件目: 正常
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskImport,
		ImportTasks: []json.RawMessage{
			json.RawMessage(`{"project_id":"proj-1","title":"t1","remote_id":"r1"}`),
			json.RawMessage(`{"project_id":"proj-1","title":"t2","behavior":"dev","remote_id":"r2"}`),
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

// --- stub types for job-related executor tests ---

type stubJobStore struct {
	jobs   map[string]*api.Job
	byTask map[string][]*api.Job
}

func (s *stubJobStore) GetJob(id string) (*api.Job, error) {
	if j, ok := s.jobs[id]; ok {
		return j, nil
	}
	return nil, fmt.Errorf("job not found: %s", id)
}

func (s *stubJobStore) ListJobsByTask(taskID string) ([]*api.Job, error) {
	return s.byTask[taskID], nil
}

func (s *stubJobStore) UpdateJob(_ *api.Job) error { return nil }

type stubJobLogReader struct {
	data map[string][]byte
	err  error
}

func (r *stubJobLogReader) ReadJobLog(runtimeID string) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	if d, ok := r.data[runtimeID]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("log not found: %s", runtimeID)
}

func (r *stubJobLogReader) StatJobLog(runtimeID string) (int64, time.Time, error) {
	if r.err != nil {
		return 0, time.Time{}, r.err
	}
	if d, ok := r.data[runtimeID]; ok {
		return int64(len(d)), time.Time{}, nil
	}
	return 0, time.Time{}, os.ErrNotExist
}

func newJobExecutor(t *testing.T) (*boidBuiltinExecutor, *capturingTaskStore, *stubJobStore) {
	t.Helper()
	ts := &capturingTaskStore{
		created: []*orchestrator.Task{
			{ID: "task-1", ProjectID: "proj-1", Status: orchestrator.TaskStatusPending},
			{ID: "task-x", ProjectID: "proj-x", Status: orchestrator.TaskStatusPending},
		},
	}
	js := &stubJobStore{
		jobs: map[string]*api.Job{
			"job-1": {ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent", Role: "gate", Status: api.JobStatusCompleted, ExitCode: 0},
			"job-x": {ID: "job-x", TaskID: "task-x", ProjectID: "proj-x", HandlerID: "run-agent", Role: "gate", Status: api.JobStatusFailed, ExitCode: 1},
		},
		byTask: map[string][]*api.Job{
			"task-1": {
				{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent", Role: "gate", Status: api.JobStatusCompleted},
				{ID: "job-2", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "auto-merge", Role: "gate", Status: api.JobStatusRunning},
			},
		},
	}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{}}
	exec := &boidBuiltinExecutor{
		tasks:     &api.TaskAppService{Tasks: ts, Meta: meta},
		jobs:      js,
		logReader: &stubJobLogReader{data: map[string][]byte{"rt-1": []byte("log line\n")}},
	}
	return exec, ts, js
}

// --- action_send executor tests ---

func TestBoidBuiltinExecutor_ActionSend_WorkspaceIsolation(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	// cross-workspace task は拒否
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:         sandbox.BoidOpActionSend,
		TaskID:     "task-x",
		ActionType: "reopen",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("expected workspace rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_ActionSend_TaskNotFound(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:         sandbox.BoidOpActionSend,
		TaskID:     "no-such-task",
		ActionType: "reopen",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("expected error for unknown task, got exit=%d", resp.ExitCode)
	}
}

func TestBoidBuiltinExecutor_ActionSend_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{workflow: nil}
	ctx := sandbox.TokenContext{ProjectID: "proj-1"}
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:         sandbox.BoidOpActionSend,
		TaskID:     "t1",
		ActionType: "reopen",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// --- job_list executor tests ---

func TestBoidBuiltinExecutor_JobList_HappyPath(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpJobList,
		TaskID: "task-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "job-1") || !strings.Contains(resp.Stdout, "job-2") {
		t.Errorf("stdout = %q, want job-1 and job-2", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_JobList_WorkspaceIsolation(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpJobList,
		TaskID: "task-x",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("expected workspace rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_JobList_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{jobs: nil}
	ctx := sandbox.TokenContext{ProjectID: "proj-1"}
	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpJobList,
		TaskID: "t1",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// --- job_show executor tests ---

func TestBoidBuiltinExecutor_JobShow_HappyPath(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpJobShow,
		JobID: "job-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "job-1") {
		t.Errorf("stdout = %q, want job-1", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "run-agent") {
		t.Errorf("stdout = %q, want run-agent in handler", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, "Status:") {
		t.Errorf("stdout = %q, want Status: field", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_JobShow_CrossProjectReject(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpJobShow,
		JobID: "job-x",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("expected workspace rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_JobShow_NotFound(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpJobShow,
		JobID: "no-such-job",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("expected error for unknown job, got exit=%d", resp.ExitCode)
	}
}

// --- job_log executor tests ---

func TestBoidBuiltinExecutor_JobLog_HappyPath(t *testing.T) {
	exec, _, js := newJobExecutor(t)
	js.jobs["job-1"].RuntimeID = "rt-1"
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpJobLog,
		JobID: "job-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "log line\n" {
		t.Errorf("stdout = %q, want log line", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_JobLog_NoRuntime(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	// job-1 の RuntimeID は空 (newJobExecutor でセットされていない)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpJobLog,
		JobID: "job-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (not available is OK)", resp.ExitCode)
	}
	if !strings.Contains(resp.Stdout, "not available") {
		t.Errorf("stdout = %q, want 'not available'", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_JobLog_CrossProjectReject(t *testing.T) {
	exec, _, _ := newJobExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpJobLog,
		JobID: "job-x",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("expected workspace rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// --- task_delete executor tests ---

func newDeleteExecutor(t *testing.T) (*boidBuiltinExecutor, *capturingTaskStore) {
	t.Helper()
	store := &capturingTaskStore{
		created: []*orchestrator.Task{
			{ID: "task-1", ProjectID: "proj-1", Status: orchestrator.TaskStatusPending},
			{ID: "task-exec", ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting},
			{ID: "task-foreign", ProjectID: "proj-x", Status: orchestrator.TaskStatusPending},
		},
	}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{}}
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: store, Meta: meta},
	}
	return exec, store
}

func TestBoidBuiltinExecutor_TaskDelete_HappyPath(t *testing.T) {
	exec, store := newDeleteExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskDelete,
		TaskID: "task-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "task-1") {
		t.Errorf("stdout = %q, want task-1", resp.Stdout)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "task-1" {
		t.Errorf("deleted = %v, want [task-1]", store.deleted)
	}
}

func TestBoidBuiltinExecutor_TaskDelete_CrossProjectReject(t *testing.T) {
	exec, store := newDeleteExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskDelete,
		TaskID: "task-foreign",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("expected workspace rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(store.deleted) != 0 {
		t.Errorf("cross-workspace delete should not reach store, deleted=%v", store.deleted)
	}
}

func TestBoidBuiltinExecutor_TaskDelete_ActiveTaskForceRequired(t *testing.T) {
	exec, store := newDeleteExecutor(t)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskDelete,
		TaskID: "task-exec",
		Force:  false,
	})
	if resp.ExitCode != 1 {
		t.Fatalf("expected error for active task without force, got exit=%d", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "active") && !strings.Contains(resp.Stderr, "force") {
		t.Errorf("stderr = %q, want 'active' or 'force' hint", resp.Stderr)
	}
	if len(store.deleted) != 0 {
		t.Errorf("active task without force should not reach store, deleted=%v", store.deleted)
	}
}

func TestBoidBuiltinExecutor_TaskDelete_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{tasks: nil}
	ctx := sandbox.TokenContext{ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskDelete,
		TaskID: "task-1",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}
