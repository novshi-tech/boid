package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestChildCount_PreExecutionChildrenStillCountAsOpen verifies open_child_count
// still counts pre-execution children (captured/triaged/parked/ready) as open,
// and excludes only dropped alongside done/aborted (Opus指摘#6: 逆輸入1で子は
// 最初から task 化しないため、pre-execution な子が親の done 主張を塞ぐ心配は
// そもそも発生しない — 緩めると verifyDoneClaim の詐称防止ガードを無意味に
// 弱めるだけになる)。
func TestChildCount_PreExecutionChildrenStillCountAsOpen(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "Parent", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusDropped,
	}
	for _, s := range statuses {
		child := &orchestrator.Task{ProjectID: "proj-1", Title: "Child", Behavior: "dev", Status: s, ParentID: parent.ID}
		if err := orchestrator.CreateTask(d.Conn, child); err != nil {
			t.Fatalf("create child %s: %v", s, err)
		}
	}

	got, err := orchestrator.GetTask(d.Conn, parent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// captured/triaged/parked/ready count as open (4); dropped does not.
	if got.OpenChildCount != 4 {
		t.Errorf("OpenChildCount = %d, want 4 (dropped excluded, pre-execution counted)", got.OpenChildCount)
	}
}

func TestListTasks_Open_ExcludesBarePreExecutionTask(t *testing.T) {
	d := createTestProject(t)
	triaged := &orchestrator.Task{ProjectID: "proj-1", Title: "Triaged, no children", Behavior: "dev", Status: orchestrator.TaskStatusTriaged}
	if err := orchestrator.CreateTask(d.Conn, triaged); err != nil {
		t.Fatalf("create: %v", err)
	}
	pending := &orchestrator.Task{ProjectID: "proj-1", Title: "Ordinary pending", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, pending); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	ids := map[string]bool{}
	for _, tk := range got {
		ids[tk.ID] = true
	}
	if ids[triaged.ID] {
		t.Errorf("open view must not include a bare (childless) pre-execution task, got it for %s", triaged.ID)
	}
	if !ids[pending.ID] {
		t.Errorf("open view must still include ordinary pending tasks")
	}
}

// TestListTasks_Open_ExcludesChildlessPreExecutionTaskWithTerminalParent は
// codex レビューで見つかったバグの回帰テスト: 親 (done) を持つ childless な
// triaged task が、祖先救済 CTE の base case が「自分自身」を含んでいたせいで
// 親の状態に関係なく open に漏れていた。CTE を「非 terminal な祖先の子孫のみ」
// に再構成して修正済み。
func TestListTasks_Open_ExcludesChildlessPreExecutionTaskWithTerminalParent(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "Done parent", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ProjectID: "proj-1", Title: "Triaged, no children", Behavior: "dev", Status: orchestrator.TaskStatusTriaged, ParentID: parent.ID}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, tk := range got {
		if tk.ID == child.ID {
			t.Fatalf("open view must not include a childless pre-execution task just because it has a parent_id (parent status: %s)", parent.Status)
		}
	}
}

func TestListTasks_Open_IncludesPreExecutionParentWithExecutingChild(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "Triaged parent", Behavior: "dev", Status: orchestrator.TaskStatusTriaged}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ProjectID: "proj-1", Title: "Dispatched child", Behavior: "dev", Status: orchestrator.TaskStatusExecuting, ParentID: parent.ID}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	ids := map[string]bool{}
	for _, tk := range got {
		ids[tk.ID] = true
	}
	if !ids[parent.ID] {
		t.Error("open view must include a triaged parent that has a live (executing) child — do not hide in-progress triage work")
	}
	if !ids[child.ID] {
		t.Error("open view must include the executing child itself")
	}
}

func TestListTasks_Closed_IncludesDropped(t *testing.T) {
	d := createTestProject(t)
	dropped := &orchestrator.Task{ProjectID: "proj-1", Title: "Dropped", Behavior: "dev", Status: orchestrator.TaskStatusDropped}
	if err := orchestrator.CreateTask(d.Conn, dropped); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := &orchestrator.Task{ProjectID: "proj-1", Title: "Done", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, done); err != nil {
		t.Fatalf("create: %v", err)
	}
	triaged := &orchestrator.Task{ProjectID: "proj-1", Title: "Triaged", Behavior: "dev", Status: orchestrator.TaskStatusTriaged}
	if err := orchestrator.CreateTask(d.Conn, triaged); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "closed"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	ids := map[string]bool{}
	for _, tk := range got {
		ids[tk.ID] = true
	}
	if !ids[dropped.ID] {
		t.Error("closed view must include dropped tasks (Opus指摘#3: otherwise invisible in both open and closed)")
	}
	if !ids[done.ID] {
		t.Error("closed view must still include done tasks")
	}
	if ids[triaged.ID] {
		t.Error("closed view must not include a pre-execution (non-terminal) task")
	}
}

func TestListTasks_Queue_ReturnsExactlyPreExecutionSet(t *testing.T) {
	d := createTestProject(t)
	included := []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusReady,
	}
	excluded := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
		orchestrator.TaskStatusDropped,
	}
	ids := map[orchestrator.TaskStatus]string{}
	for _, s := range append(append([]orchestrator.TaskStatus{}, included...), excluded...) {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: "task-" + string(s), Behavior: "dev", Status: s}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create %s: %v", s, err)
		}
		ids[s] = task.ID
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "queue"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, tk := range got {
		gotIDs[tk.ID] = true
	}
	for _, s := range included {
		if !gotIDs[ids[s]] {
			t.Errorf("queue view must include status %q", s)
		}
	}
	for _, s := range excluded {
		if gotIDs[ids[s]] {
			t.Errorf("queue view must not include status %q", s)
		}
	}
}

// TestListTasks_Open_RescuesGreatGrandchildOfOpenAncestor pins down that the
// open_descendants CTE rewrite (base case = children of a non-terminal
// parent, not the non-terminal task itself — see the self-match fix above)
// still rescues descendants several levels down, not just direct children.
// codex review round 2 asked for this explicitly since the rewrite touched
// the recursive term's seed.
func TestListTasks_Open_RescuesGreatGrandchildOfOpenAncestor(t *testing.T) {
	d := createTestProject(t)
	greatGrandparent := &orchestrator.Task{ProjectID: "proj-1", Title: "gg-parent (open)", Behavior: "dev", Status: orchestrator.TaskStatusExecuting}
	if err := orchestrator.CreateTask(d.Conn, greatGrandparent); err != nil {
		t.Fatalf("create great-grandparent: %v", err)
	}
	grandparent := &orchestrator.Task{ProjectID: "proj-1", Title: "grandparent (done)", Behavior: "dev", Status: orchestrator.TaskStatusDone, ParentID: greatGrandparent.ID}
	if err := orchestrator.CreateTask(d.Conn, grandparent); err != nil {
		t.Fatalf("create grandparent: %v", err)
	}
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "parent (done)", Behavior: "dev", Status: orchestrator.TaskStatusDone, ParentID: grandparent.ID}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ProjectID: "proj-1", Title: "child (done)", Behavior: "dev", Status: orchestrator.TaskStatusDone, ParentID: parent.ID}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	ids := map[string]bool{}
	for _, tk := range got {
		ids[tk.ID] = true
	}
	for _, tk := range []*orchestrator.Task{grandparent, parent, child} {
		if !ids[tk.ID] {
			t.Errorf("open view must rescue %q (%s) — it has a live (executing) ancestor several levels up", tk.Title, tk.ID)
		}
	}
}

// TestListTasks_Open_AllTerminalAncestors_NoRescue is the negative twin of
// the above: when every ancestor is terminal too, nothing should be rescued
// (and nothing should self-match via the CTE either).
func TestListTasks_Open_AllTerminalAncestors_NoRescue(t *testing.T) {
	d := createTestProject(t)
	grandparent := &orchestrator.Task{ProjectID: "proj-1", Title: "grandparent (aborted)", Behavior: "dev", Status: orchestrator.TaskStatusAborted}
	if err := orchestrator.CreateTask(d.Conn, grandparent); err != nil {
		t.Fatalf("create grandparent: %v", err)
	}
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "parent (done)", Behavior: "dev", Status: orchestrator.TaskStatusDone, ParentID: grandparent.ID}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ProjectID: "proj-1", Title: "child (done)", Behavior: "dev", Status: orchestrator.TaskStatusDone, ParentID: parent.ID}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, tk := range got {
		if tk.ID == grandparent.ID || tk.ID == parent.ID || tk.ID == child.ID {
			t.Errorf("open view must not include %q — every ancestor in this chain is terminal", tk.Title)
		}
	}
}
