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
// every Task/Action/CardAttrs/TaskIdentity method TxStore needs) plus
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
	if task.Status != orchestrator.TaskStatusParked {
		t.Errorf("Status = %q, want %q (I-4/J-9: new tasks always land parked — card machine v2 folds captured into parked, docs/plans/suggestion-as-state-transition-impl.md §3.5)", task.Status, orchestrator.TaskStatusParked)
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

	// The card's CardAttrs columns are readable via GetTaskTriage from the
	// moment CreateTask makes it type='card' — card-model-cleanup PR-2
	// (design doc §3.6) retired the separate "seed the sidecar after the
	// fact" step this comment used to describe: there is no sidecar table
	// left, GetTaskTriage now just reads the same tasks row back.
	tt, err := orchestrator.GetTaskTriage(svc.Tx.(realTransactor).conn, result.TaskID)
	if err != nil {
		t.Fatalf("GetTaskTriage: %v (card row is not readable as a card)", err)
	}
	if tt.TaskID != result.TaskID {
		t.Errorf("CardAttrs.TaskID = %q, want %q", tt.TaskID, result.TaskID)
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

// TestResolveOrCapture_CapturedCard_HasNoExecAttrs replaces the pre-PR-2
// TestResolveOrCapture_BaseBranchTemplate_StoredLiteralUnexpanded (card-model-
// cleanup PR-2, design doc §3.7): that test pinned a templated meta.BaseBranch
// being stored VERBATIM (unexpanded) on a freshly captured task, because the
// old ResolveOrCapture called ResolveBehavior and squirreled away a
// behavior/traits/readonly/branch_prefix/base_branch/payload/instructions set
// nothing downstream ever consumed (the "base_branch 未展開テンプレ地雷", design
// doc §7). PR-2 removes the call to ResolveBehavior entirely — a card has no
// ExecAttrs to put any of that into — so the landmine is now structurally
// impossible rather than merely unused. This test pins that directly: a
// freshly captured task's Exec is nil and its Card is set, full stop,
// regardless of what a project's meta.BaseBranch happens to contain.
func TestResolveOrCapture_CapturedCard_HasNoExecAttrs(t *testing.T) {
	svc := newResolveOrCaptureTestService(t)

	result, err := svc.ResolveOrCapture(context.Background(), ResolveOrCaptureRequest{
		ProjectID: "proj-1", Identity: "jira:TMPL-1", Title: "t",
	})
	if err != nil {
		t.Fatalf("ResolveOrCapture() error = %v", err)
	}
	task, err := orchestrator.GetTask(svc.Tx.(realTransactor).conn, result.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Type != orchestrator.TaskTypeCard {
		t.Errorf("Type = %q, want %q", task.Type, orchestrator.TaskTypeCard)
	}
	if task.Exec != nil {
		t.Errorf("Exec = %+v, want nil — a card structurally has no ExecAttrs (no behavior/base_branch/payload/... to leave dangling)", task.Exec)
	}
	if task.Card == nil {
		t.Fatal("Card is nil, want a non-nil CardAttrs")
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
	// card-model-cleanup PR-2: TaskStatusTriaged no longer exists (design doc
	// §3.3, migration 0045) — card machine v2 already folded it into parked
	// before this PR; parked is the substitute "not-yet-triaged card" status.
	existing := &orchestrator.Task{ID: "existing-task", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
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

// ---- real-DB rollback proof (review 推奨5) ----

// failingLinkIdentityTxStore wraps the SAME real orchestrator.TaskRepository
// (real sqlite tx) realTaskRepoTxStore does, and only overrides LinkIdentity
// to force a failure. CreateTask / ResolveIdentity all run for real against
// sqlite — unlike mockROCTxStore above (used only for the
// conflict-propagation test), this exercises the ACTUAL rollback rather than
// just proving WithinTx was called exactly once.
type failingLinkIdentityTxStore struct {
	realTaskRepoTxStore
}

func (failingLinkIdentityTxStore) LinkIdentity(projectID, identity, taskID string) error {
	return fmt.Errorf("forced LinkIdentity failure for rollback test")
}

type failingLinkIdentityTransactor struct{ conn *sql.DB }

func (t failingLinkIdentityTransactor) WithinTx(fn func(TxStore) error) error {
	return db.InTxDB(t.conn, func(tx db.DBTX) error {
		return fn(failingLinkIdentityTxStore{realTaskRepoTxStore{orchestrator.NewTaskRepository(tx)}})
	})
}

// TestResolveOrCapture_LinkIdentityFailure_RollsBackCreateAndSeed pins the
// doc's 検証項目 "新規作成に失敗したら identity も残らない (トランザクション
// 境界)" against a REAL sqlite transaction, not just the structural
// WithinTx-call-count proof TestResolveOrCapture_
// ConflictingIdentity_PropagatesErrIdentityConflict gives (that test uses a
// mock TxStore whose CreateTask is a simple stub with no rollback semantics
// of its own — it proves ONE WithinTx call happened, not that a real commit
// never landed).
//
// What this catches that the mock-based test can't: a regression where
// db.InTxDB's rollback-on-error path silently breaks (e.g. a future change
// that swallows fn's error before returning it, or calls tx.Commit()
// unconditionally) would make the mock-based test still pass (WithinTx is
// still called once, LinkIdentity is still called once) while this test
// fails, because it would find the orphan task row actually committed to
// disk. card-model-cleanup PR-2 (migration 0045) folded the old task_triage
// sidecar table into tasks itself and dropped the table outright, so a
// card's kind/urgency/.../detail columns rolling back is no longer a
// separate check from the tasks row rolling back — one COUNT(*) covers both.
func TestResolveOrCapture_LinkIdentityFailure_RollsBackCreateAndSeed(t *testing.T) {
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

	svc := &TaskWorkflowService{
		Tx: failingLinkIdentityTransactor{conn: d.Conn},
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			DefaultTaskBehavior: "triage",
			TaskBehaviors:       map[string]orchestrator.TaskBehavior{"triage": {}},
		}},
	}

	_, err = svc.ResolveOrCapture(context.Background(), ResolveOrCaptureRequest{
		ProjectID: "proj-1", Identity: "jira:ROLLBACK-1", Title: "t", Description: "d",
	})
	if err == nil {
		t.Fatal("ResolveOrCapture() error = nil, want the forced LinkIdentity failure to propagate")
	}

	var taskCount, identityCount int
	if err := d.Conn.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := d.Conn.QueryRow("SELECT COUNT(*) FROM task_identities").Scan(&identityCount); err != nil {
		t.Fatalf("count task_identities: %v", err)
	}
	if taskCount != 0 {
		t.Errorf("tasks rows = %d, want 0 (CreateTask — including the card's own kind/urgency/wake_at/.../detail columns, folded into this same row by migration 0045 — must have been rolled back with LinkIdentity's failure)", taskCount)
	}
	if identityCount != 0 {
		t.Errorf("task_identities rows = %d, want 0 (the forced-failing LinkIdentity itself must not have left a row)", identityCount)
	}

	// Also confirm via the public API: a second call sees an unresolved
	// identity, exactly as if the first call never happened.
	if _, err := orchestrator.ResolveIdentity(d.Conn, "proj-1", "jira:ROLLBACK-1"); !errors.Is(err, orchestrator.ErrTaskNotFound) {
		t.Errorf("ResolveIdentity after rollback: err = %v, want ErrTaskNotFound (no trace of the rolled-back attempt)", err)
	}
}
