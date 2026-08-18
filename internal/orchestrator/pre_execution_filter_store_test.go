package orchestrator_test

import (
	"testing"
	"time"

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

// TestListTasks_Parked_ReturnsOnlyParkedTasks is the store-side half of the
// wiring-seams #28 pin (BD-8 post-merge review): the Parked tab (filters.templ,
// web.go) sends `status=parked` as a bare string literal — store.go has no
// dedicated "parked" branch, it falls through to the generic
// `else if filter.Status != "" { ... t.status = ? }` case alongside every
// other status value store.go doesn't special-case. Every other view already
// has a store-level pin (TestListTasks_Queue_ReturnsExactlyPreExecutionSet /
// TestListTasks_QueueNext_MembershipAndOrdering / TestListTasks_Triage_
// ReturnsPreExecutionPlusWorking / TestListTasks_Closed_IncludesDropped
// above) — parked did not, until now. Without this, a future dedicated
// "parked" branch (or a typo in one) could silently narrow or widen the
// result set: no 500, no panic, just a wrong list (決定9 の再発).
func TestListTasks_Parked_ReturnsOnlyParkedTasks(t *testing.T) {
	d := createTestProject(t)
	// Includes captured/dropped specifically (Opus review finding,
	// 2026-08-18): a future dedicated "parked" branch written as a
	// superset — e.g. copying queue's pre-execution set (captured/
	// triaged/parked/ready) instead of an exact-status match — would pass
	// this test undetected without them in the fixture.
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
	}
	ids := map[orchestrator.TaskStatus]string{}
	for _, s := range statuses {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: "task-" + string(s), Behavior: "dev", Status: s}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create %s: %v", s, err)
		}
		ids[s] = task.ID
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "parked"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 || got[0].ID != ids[orchestrator.TaskStatusParked] {
		gotIDs := make([]string, len(got))
		for i, tk := range got {
			gotIDs[i] = tk.ID + ":" + string(tk.Status)
		}
		t.Fatalf(`ListTasks(Status: "parked") = %v, want exactly [%s] (the parked task only)`, gotIDs, ids[orchestrator.TaskStatusParked])
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

// TestListTasks_Open_IncludesWorking / TestListTasks_Queue_ExcludesWorking
// pin down PR-2's deliberate classification of TaskStatusWorking as
// execution-like rather than pre-execution (see TaskStatusWorking's own doc
// comment in model.go): a childless working task must behave exactly like a
// childless executing task for filtering purposes — visible in "open"
// (unlike triaged/ready/etc, which need a live child to be rescued),
// invisible in "queue" (queue is for things nose still needs to respond to;
// working has already been Go'd).
func TestListTasks_Open_IncludesWorking(t *testing.T) {
	d := createTestProject(t)
	working := &orchestrator.Task{ProjectID: "proj-1", Title: "Working, no children", Behavior: "dev", Status: orchestrator.TaskStatusWorking}
	if err := orchestrator.CreateTask(d.Conn, working); err != nil {
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
	if !ids[working.ID] {
		t.Errorf("open view must include a childless working task, same as an executing task")
	}
}

func TestListTasks_Queue_ExcludesWorking(t *testing.T) {
	d := createTestProject(t)
	working := &orchestrator.Task{ProjectID: "proj-1", Title: "Working", Behavior: "dev", Status: orchestrator.TaskStatusWorking}
	if err := orchestrator.CreateTask(d.Conn, working); err != nil {
		t.Fatalf("create: %v", err)
	}
	ready := &orchestrator.Task{ProjectID: "proj-1", Title: "Ready", Behavior: "dev", Status: orchestrator.TaskStatusReady}
	if err := orchestrator.CreateTask(d.Conn, ready); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "queue"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	ids := map[string]bool{}
	for _, tk := range got {
		ids[tk.ID] = true
	}
	if ids[working.ID] {
		t.Errorf("queue view must not include a working task — it has already been Go'd")
	}
	if !ids[ready.ID] {
		t.Errorf("queue view must still include a ready (pre-execution) task")
	}
}

// TestListTasks_QueueNext_MembershipAndOrdering pins down PR-3's queue の決定論的評価
// rules 2/3: state ∈ {ready, triaged} かつ urgency ∈ {now, today, week}
// (someday/empty excluded), ordered by urgency (now>today>week) then state
// (ready before triaged) then created_at ascending then id. This is
// deliberately a NEW filter value ("queue_next"), distinct from the existing
// "queue" keyword (PR-1's preExecutionStatusSQLList-based broader superset,
// pinned by TestListTasks_Queue_ReturnsExactlyPreExecutionSet above) — see
// docs/plans/cross-project-issue-triage.md PR-3 scoping notes for why "queue"
// itself is left unchanged.
func TestListTasks_QueueNext_MembershipAndOrdering(t *testing.T) {
	d := createTestProject(t)

	mk := func(title string, status orchestrator.TaskStatus, urgency string) *orchestrator.Task {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: title, Behavior: "dev", Status: status}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if urgency != "" {
			if err := orchestrator.UpsertTaskTriage(d.Conn, &orchestrator.TaskTriage{TaskID: task.ID, Urgency: urgency}); err != nil {
				t.Fatalf("upsert task_triage %s: %v", title, err)
			}
		}
		time.Sleep(2 * time.Millisecond) // force distinct created_at for ordering
		return task
	}

	// Created oldest-first as triaged, then ready, then today, then week — so
	// the assertions below only pass if state-rank (ready before triaged)
	// wins over created_at, and urgency-rank wins over both.
	triagedNowOlder := mk("triaged-now-older", orchestrator.TaskStatusTriaged, orchestrator.UrgencyNow)
	readyNow := mk("ready-now", orchestrator.TaskStatusReady, orchestrator.UrgencyNow)
	triagedToday := mk("triaged-today", orchestrator.TaskStatusTriaged, orchestrator.UrgencyToday)
	readyWeek := mk("ready-week", orchestrator.TaskStatusReady, orchestrator.UrgencyWeek)
	_ = mk("triaged-someday", orchestrator.TaskStatusTriaged, orchestrator.UrgencySomeday)
	_ = mk("ready-no-urgency", orchestrator.TaskStatusReady, "")
	_ = mk("parked-now", orchestrator.TaskStatusParked, orchestrator.UrgencyNow)
	_ = mk("captured-now", orchestrator.TaskStatusCaptured, orchestrator.UrgencyNow)
	_ = mk("working-now", orchestrator.TaskStatusWorking, orchestrator.UrgencyNow)
	_ = mk("done-now", orchestrator.TaskStatusDone, orchestrator.UrgencyNow)

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "queue_next"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}

	var gotIDs []string
	for _, tk := range got {
		gotIDs = append(gotIDs, tk.ID)
	}
	// Ordering: now (ready first, then triaged, oldest first) > today > week.
	want := []string{readyNow.ID, triagedNowOlder.ID, triagedToday.ID, readyWeek.ID}
	if len(gotIDs) != len(want) {
		t.Fatalf("queue_next returned %d tasks, want %d: got %v", len(gotIDs), len(want), gotIDs)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("position %d: got task id %s, want %s (order: %v)", i, gotIDs[i], want[i], gotIDs)
		}
	}
}

// TestListTasks_Triage_ReturnsPreExecutionPlusWorking pins ListTriage's default
// floor (Phase 1 PR-5a): "the live triage cards" = pre-execution ∪ working.
// Without a floor an unfiltered triage listing degrades into a full scan of
// every task row ever created plus one sidecar point query per row; "queue"
// alone cannot serve as that floor because it omits working, which is where a
// card spends the whole time its children are running.
func TestListTasks_Triage_ReturnsPreExecutionPlusWorking(t *testing.T) {
	d := createTestProject(t)
	included := []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusWorking,
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

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "triage"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, tk := range got {
		gotIDs[tk.ID] = true
	}
	for _, s := range included {
		if !gotIDs[ids[s]] {
			t.Errorf("status %q missing from the triage filter", s)
		}
	}
	for _, s := range excluded {
		if gotIDs[ids[s]] {
			t.Errorf("status %q must not appear in the triage filter", s)
		}
	}
}
