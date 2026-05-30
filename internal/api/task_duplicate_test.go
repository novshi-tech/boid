package api_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestDuplicateTask_CreatesNewTask(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "dup-proj", "Dup Project")

	// ソースタスクを作成
	var source orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id":  "dup-proj",
		"title":       "Source Task",
		"description": "Source description",
		"behavior":    "planning",
	}, &source); err != nil {
		t.Fatalf("create source task: %v", err)
	}

	// 複製
	var dup orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+source.ID+"/duplicate", map[string]any{
		"auto_start": false,
	}, &dup); err != nil {
		t.Fatalf("duplicate task: %v", err)
	}

	if dup.ID == "" {
		t.Fatal("duplicated task ID should not be empty")
	}
	if dup.ID == source.ID {
		t.Error("duplicated task should have a different ID")
	}
	if dup.Title != source.Title {
		t.Errorf("Title = %q, want %q", dup.Title, source.Title)
	}
	if dup.Description != source.Description {
		t.Errorf("Description = %q, want %q", dup.Description, source.Description)
	}
	if dup.ProjectID != source.ProjectID {
		t.Errorf("ProjectID = %q, want %q", dup.ProjectID, source.ProjectID)
	}
	if dup.Behavior != source.Behavior {
		t.Errorf("Behavior = %q, want %q", dup.Behavior, source.Behavior)
	}
	if dup.Status != orchestrator.TaskStatusPending {
		t.Errorf("Status = %q, want %q", dup.Status, orchestrator.TaskStatusPending)
	}
}

func TestDuplicateTask_CarriesRemoteIDAndInstructions(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "dup-rid-proj", "Dup RemoteID Project")

	// ソース: remote_id と instructions(リリース上書き相当)を持つ
	var source orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id":   "dup-rid-proj",
		"title":        "Source With RemoteID",
		"behavior":     "planning",
		"remote_id":    "BGO-170",
		"instructions": []map[string]any{{"type": "execution", "agent": "claude-code", "message": "release policy: push + PR"}},
	}, &source); err != nil {
		t.Fatalf("create source task: %v", err)
	}

	var dup orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+source.ID+"/duplicate", map[string]any{
		"auto_start": false,
	}, &dup); err != nil {
		t.Fatalf("duplicate task: %v", err)
	}

	// remote_id を引き継ぐこと(これが無いと feature/${TASK_REMOTE_ID} テンプレが解決できず複製が失敗していた)
	if dup.RemoteID != source.RemoteID {
		t.Errorf("RemoteID = %q, want %q", dup.RemoteID, source.RemoteID)
	}
	// instructions(リリース上書き)を引き継ぐこと
	if len(dup.Instructions) == 0 {
		t.Fatalf("duplicated task should carry source instructions, got none")
	}
	if dup.Instructions[0].Message != source.Instructions[0].Message {
		t.Errorf("Instructions[0].Message = %q, want %q", dup.Instructions[0].Message, source.Instructions[0].Message)
	}
}

// TestDuplicateTask_SourceWithRefDoesNotCollide guards the regression where a
// duplicate copied the source's ref verbatim. A source carrying a non-empty ref
// (e.g. a re-duplicated supervisor) sits in the partial unique index
// idx_tasks_ref_parent(ref, parent_id) WHERE ref != ''. Copying that ref into the
// duplicate, which shares the source's (root) parent_id, violated the index and
// failed the duplicate outright. The duplicate must instead get its own ref scope.
func TestDuplicateTask_SourceWithRefDoesNotCollide(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "dup-ref-proj", "Dup Ref Project")

	// ソース: 非空 ref を持つ root タスク (再複製された supervisor 相当)。
	var source orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "dup-ref-proj",
		"title":      "Source With Ref",
		"behavior":   "planning",
		"remote_id":  "BGO-195",
		"ref":        "warm_jacobi",
	}, &source); err != nil {
		t.Fatalf("create source task: %v", err)
	}
	if source.Ref != "warm_jacobi" {
		t.Fatalf("source Ref = %q, want %q (precondition)", source.Ref, "warm_jacobi")
	}

	// 複製が UNIQUE(ref, parent_id) 制約で失敗しないこと。
	var dup orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+source.ID+"/duplicate", map[string]any{
		"auto_start": false,
	}, &dup); err != nil {
		t.Fatalf("duplicate task with non-empty source ref: %v", err)
	}
	if dup.ID == "" || dup.ID == source.ID {
		t.Fatalf("duplicated task should have a fresh ID, got %q (source %q)", dup.ID, source.ID)
	}
	// 複製はソースの ref を継がない (継ぐと衝突するため)。root なので ref は空のまま。
	if dup.Ref == source.Ref {
		t.Errorf("duplicate inherited source ref %q; expected a fresh/empty ref scope", dup.Ref)
	}
	// remote_id は引き継ぐ (一課題に複数タスクは正常)。
	if dup.RemoteID != source.RemoteID {
		t.Errorf("RemoteID = %q, want %q", dup.RemoteID, source.RemoteID)
	}
}

func TestDuplicateTask_NotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	if err := ts.Client.Do("POST", "/api/tasks/nonexistent/duplicate", map[string]any{
		"auto_start": false,
	}, nil); err == nil {
		t.Fatal("expected error for nonexistent task, got nil")
	}
}

func TestDuplicateTask_VerifyListed(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "dup-list-proj", "Dup List Project")

	var source orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "dup-list-proj",
		"title":      "Original",
		"behavior":   "planning",
	}, &source); err != nil {
		t.Fatalf("create source task: %v", err)
	}

	var dup orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+source.ID+"/duplicate", map[string]any{
		"auto_start": false,
	}, &dup); err != nil {
		t.Fatalf("duplicate task: %v", err)
	}

	// 一覧に両タスクが存在すること
	var tasks []orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks", nil, &tasks); err != nil {
		t.Fatalf("list tasks: %v", err)
	}

	ids := make(map[string]bool)
	for _, tk := range tasks {
		ids[tk.ID] = true
	}
	if !ids[source.ID] {
		t.Errorf("source task %s not found in list", source.ID)
	}
	if !ids[dup.ID] {
		t.Errorf("duplicated task %s not found in list", dup.ID)
	}
}
