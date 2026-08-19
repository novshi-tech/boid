package api

// docs/plans/ingestion-identity.md PR-2 (B-2): 「検証」節を直接 pin する。
//
//   - 未登録キー → captured な task が 1 枚できて identity が link される。
//     created: true
//   - 同じキーで再度呼ぶ → 同じ task を返し、2 枚目を作らない。created: false
//   - 別 task に紐付いたキー → ErrIdentityConflict で、task は作られない
//   - 新規作成に失敗したら identity も残らない (トランザクション境界)
//   - 上限超過 → エラーになり、切り詰められた行が残らない (content_size_
//     entry_points_test.go 側で一般則を、ここでは resolve-or-capture 固有の
//     経路を確認する)
//
// TestResolveOrCapture_UnregisteredIdentity_* / TestResolveOrCapture_
// SameIdentityAgain_* は real sqlite (db.Open(":memory:") + migrate.Apply,
// web_management_test.go と同じ idiom — testutil はここから import すると
// internal/server 経由で循環 import になる) 上で orchestrator.TaskRepository
// を直に使う — recordingTxStore (apply_action_phase1_test.go) は CreateTask
// が task.ID を書き戻さない・ロールバックを模擬しない簡易スタブなので、
// 「2 枚目を作らない」「トランザクション境界」を実物の DB 制約
// (UNIQUE(project_id, identity)) で検証するにはここでは向かない。
// TestResolveOrCapture_ConflictingIdentity_PropagatesErrIdentityConflict だけ
// は decision の伝播 (エラー種別・WithinTx が 1 回だけ呼ばれること) を見れば
// 足りるので mock を使う。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// realTaskRepoTxStore adapts *orchestrator.TaskRepository (which implements
// every Task/Action/TaskTriage/TaskIdentity method TxStore needs) plus
// trivial Job stubs (unused by ResolveOrCapture) into a full TxStore, backed
// by a REAL sqlite transaction — the same shape internal/server/api_store.go's
// apiTxStore uses in production, just assembled locally so this package
// doesn't need to import internal/server (would invert the layer direction).
type realTaskRepoTxStore struct {
	*orchestrator.TaskRepository
}

func (realTaskRepoTxStore) GetJob(id string) (*Job, error) {
	return nil, fmt.Errorf("not implemented")
}
func (realTaskRepoTxStore) ListJobsByTask(taskID string) ([]*Job, error) { return nil, nil }
func (realTaskRepoTxStore) UpdateJob(job *Job) error                     { return nil }

type realTransactor struct{ conn *sql.DB }

func (t realTransactor) WithinTx(fn func(TxStore) error) error {
	return db.InTxDB(t.conn, func(tx db.DBTX) error {
		return fn(realTaskRepoTxStore{orchestrator.NewTaskRepository(tx)})
	})
}

func newResolveOrCaptureTestService(t *testing.T) *TaskWorkflowService {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// tasks.project_id is a real FK (migration 0021 pattern) — every project
	// this test file references must exist as a row.
	for _, id := range []string{"proj-1", "proj-a", "proj-b"} {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
	}
	return &TaskWorkflowService{
		Tx: realTransactor{conn: d.Conn},
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			DefaultTaskBehavior: "triage",
			TaskBehaviors:       map[string]orchestrator.TaskBehavior{"triage": {}},
			BaseBranch:          "main",
		}},
	}
}

func TestResolveOrCapture_UnregisteredIdentity_CreatesCapturedTaskAndLinks(t *testing.T) {
	svc := newResolveOrCaptureTestService(t)

	result, err := svc.ResolveOrCapture(context.Background(), ResolveOrCaptureRequest{
		ProjectID:   "proj-1",
		Identity:    "jira:ROOKPF-1",
		Title:       "ROOKPF-1: something broke",
		Description: "the body",
	})
	if err != nil {
		t.Fatalf("ResolveOrCapture() error = %v", err)
	}
	if !result.Created {
		t.Error("Created = false, want true for an unregistered identity")
	}
	if result.TaskID == "" {
		t.Fatal("TaskID is empty")
	}

	task, err := orchestrator.GetTask(svc.Tx.(realTransactor).conn, result.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != orchestrator.TaskStatusCaptured {
		t.Errorf("Status = %q, want %q (I-4/J-9: new tasks always land captured)", task.Status, orchestrator.TaskStatusCaptured)
	}
	if task.Title != "ROOKPF-1: something broke" || task.Description != "the body" {
		t.Errorf("Title/Description not carried through: %q / %q", task.Title, task.Description)
	}

	// identity really is linked — a resolve for the same key finds it.
	resolved, err := orchestrator.ResolveIdentity(svc.Tx.(realTransactor).conn, "proj-1", "jira:ROOKPF-1")
	if err != nil {
		t.Fatalf("ResolveIdentity after create: %v", err)
	}
	if resolved.ID != result.TaskID {
		t.Errorf("ResolveIdentity returned task %q, want %q", resolved.ID, result.TaskID)
	}

	// task_triage sidecar was seeded (needed for the captured task to show
	// up as a triage task / on the Web UI list, not just a bare task row).
	tt, err := orchestrator.GetTaskTriage(svc.Tx.(realTransactor).conn, result.TaskID)
	if err != nil {
		t.Fatalf("GetTaskTriage: %v (task_triage row was not seeded)", err)
	}
	if tt.TaskID != result.TaskID {
		t.Errorf("task_triage.TaskID = %q, want %q", tt.TaskID, result.TaskID)
	}
}

func TestResolveOrCapture_SameIdentityAgain_ReturnsSameTaskNoSecondCreate(t *testing.T) {
	svc := newResolveOrCaptureTestService(t)
	ctx := context.Background()
	req := ResolveOrCaptureRequest{ProjectID: "proj-1", Identity: "slack:1785803139.540489", Title: "t", Description: "d"}

	first, err := svc.ResolveOrCapture(ctx, req)
	if err != nil {
		t.Fatalf("first ResolveOrCapture() error = %v", err)
	}
	if !first.Created {
		t.Fatal("first call: Created = false, want true")
	}

	second, err := svc.ResolveOrCapture(ctx, req)
	if err != nil {
		t.Fatalf("second ResolveOrCapture() error = %v", err)
	}
	if second.Created {
		t.Error("second call: Created = true, want false (must resolve, not create a 2nd task)")
	}
	if second.TaskID != first.TaskID {
		t.Errorf("second call returned task %q, want the same task %q", second.TaskID, first.TaskID)
	}

	// Exactly one task exists in the project — no orphan/duplicate captured.
	tasks, err := orchestrator.ListTasks(svc.Tx.(realTransactor).conn, orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		ids := make([]string, len(tasks))
		for i, tk := range tasks {
			ids[i] = tk.ID
		}
		t.Fatalf("tasks in proj-1 = %v (%d), want exactly 1", ids, len(tasks))
	}
}

func TestResolveOrCapture_DifferentProjectsCanReuseTheSameIdentityString(t *testing.T) {
	// I-3: identity scope is per-project, mirroring PR-1's own store-level
	// test — resolve-or-capture must not accidentally widen the scope to be
	// workspace/global.
	svc := newResolveOrCaptureTestService(t)
	ctx := context.Background()

	a, err := svc.ResolveOrCapture(ctx, ResolveOrCaptureRequest{ProjectID: "proj-a", Identity: "jira:SAME-1", Title: "a"})
	if err != nil {
		t.Fatalf("proj-a ResolveOrCapture() error = %v", err)
	}
	b, err := svc.ResolveOrCapture(ctx, ResolveOrCaptureRequest{ProjectID: "proj-b", Identity: "jira:SAME-1", Title: "b"})
	if err != nil {
		t.Fatalf("proj-b ResolveOrCapture() error = %v", err)
	}
	if !a.Created || !b.Created {
		t.Fatalf("both should be fresh captures: a.Created=%v b.Created=%v", a.Created, b.Created)
	}
	if a.TaskID == b.TaskID {
		t.Error("same identity string in two different projects must resolve to two different tasks")
	}
}

func TestResolveOrCapture_DescriptionOverLimit_RejectsAndCreatesNothing(t *testing.T) {
	svc := newResolveOrCaptureTestService(t)
	oversized := strings.Repeat("a", orchestrator.MaxContentBytes+1)

	_, err := svc.ResolveOrCapture(context.Background(), ResolveOrCaptureRequest{
		ProjectID: "proj-1", Identity: "jira:BIG-1", Title: "t", Description: oversized,
	})
	if err == nil {
		t.Fatal("ResolveOrCapture() with an oversized description = nil error, want a rejection")
	}
	var sizeErr *orchestrator.ContentSizeError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("error = %v, want a *orchestrator.ContentSizeError", err)
	}

	tasks, err := orchestrator.ListTasks(svc.Tx.(realTransactor).conn, orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks created despite the size rejection: %d", len(tasks))
	}
	if _, err := orchestrator.ResolveIdentity(svc.Tx.(realTransactor).conn, "proj-1", "jira:BIG-1"); !errors.Is(err, orchestrator.ErrTaskNotFound) {
		t.Errorf("identity got linked despite the size rejection: err = %v", err)
	}
}

// TestResolveOrCapture_BaseBranchTemplate_StoredLiteralUnexpanded pins the
// ResolveOrCapture doc comment's explicit claim (task_resolve_or_capture.go):
// a templated meta.BaseBranch is stored on a freshly captured task VERBATIM,
// with neither ExpandTaskBaseBranch (${TASK_REMOTE_ID}) nor ExpandBaseBranch
// (${current_branch}) applied — unlike TaskAppService.CreateTask, which would
// 400 on this exact template (task_create.go: ExpandTaskBaseBranch fails when
// a template references ${TASK_REMOTE_ID} but remoteID is empty, and a
// ResolveOrCapture request never carries a RemoteID at all). This is safe
// only because a captured task can never reach executing directly
// (machine.go), so nothing ever consumes this literal value — see the doc
// comment for the full argument.
func TestResolveOrCapture_BaseBranchTemplate_StoredLiteralUnexpanded(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	const template = "feature/${TASK_REMOTE_ID}"
	svc := &TaskWorkflowService{
		Tx: realTransactor{conn: d.Conn},
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			DefaultTaskBehavior: "triage",
			TaskBehaviors:       map[string]orchestrator.TaskBehavior{"triage": {}},
			BaseBranch:          template,
		}},
	}

	result, err := svc.ResolveOrCapture(context.Background(), ResolveOrCaptureRequest{
		ProjectID: "proj-1", Identity: "jira:TMPL-1", Title: "t",
	})
	// The point of this test: no RemoteID is available, yet this must NOT
	// error the way CreateTask's ExpandTaskBaseBranch would — the template is
	// left untouched instead.
	if err != nil {
		t.Fatalf("ResolveOrCapture() error = %v, want success (template stays unexpanded, not an error)", err)
	}
	task, err := orchestrator.GetTask(d.Conn, result.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.BaseBranch != template {
		t.Errorf("task.BaseBranch = %q, want the literal unexpanded template %q", task.BaseBranch, template)
	}
}

// ---- conflict propagation (mocked TxStore — see file doc comment for why) ----

// mockROCTxStore implements just the TxStore methods ResolveOrCapture calls;
// every other method panics via the nil-embedded TxStore if invoked, which
// would mean ResolveOrCapture started depending on something new — a loud,
// intentional failure rather than a silent no-op.
type mockROCTxStore struct {
	TxStore

	resolveIdentityErr  error
	resolveIdentityTask *orchestrator.Task
	linkIdentityErr     error

	createTaskCalls []*orchestrator.Task
	seedCalls       []string
	linkCalls       int
}

func (m *mockROCTxStore) ResolveIdentity(projectID, identity string) (*orchestrator.Task, error) {
	if m.resolveIdentityErr != nil {
		return nil, m.resolveIdentityErr
	}
	return m.resolveIdentityTask, nil
}

func (m *mockROCTxStore) CreateTask(task *orchestrator.Task) error {
	if task.ID == "" {
		task.ID = "mock-created-task"
	}
	m.createTaskCalls = append(m.createTaskCalls, task)
	return nil
}

func (m *mockROCTxStore) SeedTaskTriage(taskID string) error {
	m.seedCalls = append(m.seedCalls, taskID)
	return nil
}

func (m *mockROCTxStore) LinkIdentity(projectID, identity, taskID string) error {
	m.linkCalls++
	return m.linkIdentityErr
}

type countingTransactor struct {
	store     TxStore
	withinTxN int
}

func (t *countingTransactor) WithinTx(fn func(TxStore) error) error {
	t.withinTxN++
	return fn(t.store)
}

func TestResolveOrCapture_ConflictingIdentity_PropagatesErrIdentityConflict(t *testing.T) {
	store := &mockROCTxStore{
		resolveIdentityErr: orchestrator.ErrTaskNotFound, // not yet resolved from THIS call's point of view
		linkIdentityErr:    orchestrator.ErrIdentityConflict,
	}
	tx := &countingTransactor{store: store}
	svc := &TaskWorkflowService{
		Tx: tx,
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			DefaultTaskBehavior: "triage",
			TaskBehaviors:       map[string]orchestrator.TaskBehavior{"triage": {}},
		}},
	}

	result, err := svc.ResolveOrCapture(context.Background(), ResolveOrCaptureRequest{
		ProjectID: "proj-1", Identity: "jira:TAKEN-1", Title: "t",
	})
	if result != nil {
		t.Errorf("result = %+v, want nil on conflict", result)
	}
	if !errors.Is(err, orchestrator.ErrIdentityConflict) {
		t.Fatalf("error = %v, want errors.Is(_, orchestrator.ErrIdentityConflict)", err)
	}

	// Structural atomicity proof: create + link happened inside exactly ONE
	// WithinTx call (not two separate transactions with a commit in
	// between) — combined with db.InTxDB's own established rollback-on-error
	// behavior, this is what guarantees the create above never commits on
	// its own when the link that follows it fails.
	if tx.withinTxN != 1 {
		t.Errorf("WithinTx called %d times, want exactly 1", tx.withinTxN)
	}
	if len(store.createTaskCalls) != 1 {
		t.Errorf("CreateTask called %d times, want 1 (a create WAS attempted before the conflicting link)", len(store.createTaskCalls))
	}
	if store.linkCalls != 1 {
		t.Errorf("LinkIdentity called %d times, want 1", store.linkCalls)
	}
}

func TestResolveOrCapture_AlreadyResolved_DoesNotAttemptCreate(t *testing.T) {
	existing := &orchestrator.Task{ID: "existing-task", ProjectID: "proj-1", Status: orchestrator.TaskStatusTriaged}
	store := &mockROCTxStore{resolveIdentityTask: existing}
	tx := &countingTransactor{store: store}
	svc := &TaskWorkflowService{Tx: tx}

	result, err := svc.ResolveOrCapture(context.Background(), ResolveOrCaptureRequest{
		ProjectID: "proj-1", Identity: "jira:EXISTS-1",
	})
	if err != nil {
		t.Fatalf("ResolveOrCapture() error = %v", err)
	}
	if result.Created {
		t.Error("Created = true, want false — identity already resolved")
	}
	if result.TaskID != existing.ID {
		t.Errorf("TaskID = %q, want %q", result.TaskID, existing.ID)
	}
	if len(store.createTaskCalls) != 0 {
		t.Errorf("CreateTask called %d times, want 0 (already resolved, must not attempt to create)", len(store.createTaskCalls))
	}
	if store.linkCalls != 0 {
		t.Errorf("LinkIdentity called %d times, want 0", store.linkCalls)
	}
}
