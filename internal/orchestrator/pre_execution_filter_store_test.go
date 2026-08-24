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

// TestListTasks_Open_IncludesBareCapturedTask pins
// docs/plans/ingestion-identity.md PR-2 (B-2)'s "captured の可視化"
// requirement —実物確認の結果、captured 単体タスクはどの既存タブ
// (open/closed/queue_next/parked) からも見えなかった (queue_next は明示的に
// captured を除外、parked は状態が違う、closed は非終端を除外)。この状態を
// 直したのが notOpenSelfStatusSQLList の carve-out。captured は「起票に値す
// ると workspace が既に判断したが、まだ index が外れて棚上げされている」状態
// (I-4) であり、triaged/parked/ready (Queue/Parked タブが既に受け持つ) とは
// 意味が違う — triaged/parked/ready の除外は変えない (このテストの
// triaged サブケースが回帰を防ぐ)。
func TestListTasks_Open_IncludesBareCapturedTask(t *testing.T) {
	d := createTestProject(t)
	captured := &orchestrator.Task{ProjectID: "proj-1", Title: "Captured, no children", Behavior: "dev", Status: orchestrator.TaskStatusCaptured}
	if err := orchestrator.CreateTask(d.Conn, captured); err != nil {
		t.Fatalf("create: %v", err)
	}
	triaged := &orchestrator.Task{ProjectID: "proj-1", Title: "Triaged, no children", Behavior: "dev", Status: orchestrator.TaskStatusTriaged}
	if err := orchestrator.CreateTask(d.Conn, triaged); err != nil {
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
	if !ids[captured.ID] {
		t.Errorf("open view must include a bare captured task (PR-2 B-2 visibility), got %v", ids)
	}
	if ids[triaged.ID] {
		t.Errorf("open view must still NOT include a bare triaged task (Queue tab owns it) — got it for %s", triaged.ID)
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

// TestListTasks_Open_ExcludesBareParkedTask pins PR-2's review conclusion on
// notOpenSelfStatusSQLList's captured carve-out (docs/plans/
// suggestion-as-state-transition-impl.md §4.1's "Open タブに何が出るべきか"
// question): under card machine v2, EVERY new card lands directly in
// "parked" (task_resolve_or_capture.go / task_create.go, PR-1) — captured is
// legacy-only now. Unlike captured circa ingestion-identity.md PR-2 (which
// had NO dedicated tab at all, hence the Open carve-out), parked already has
// its own first-class tab (Parked, filters.templ). So a bare (childless)
// parked task must stay excluded from Open the same way triaged/ready
// already are — the carve-out does not need widening to parked, because
// design doc §3.6's "queue は唯一の窓ではない" requirement is already met by
// the Parked tab. This is a NEW pin (no prior test exercised a bare parked
// task against the "open" filter) confirming the decision to leave
// notOpenSelfStatusSQLList's shape unchanged.
func TestListTasks_Open_ExcludesBareParkedTask(t *testing.T) {
	d := createTestProject(t)
	parked := &orchestrator.Task{ProjectID: "proj-1", Title: "Parked, no children", Behavior: "dev", Status: orchestrator.TaskStatusParked}
	if err := orchestrator.CreateTask(d.Conn, parked); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, tk := range got {
		if tk.ID == parked.ID {
			t.Fatalf("open view must not include a bare parked task (%s) — Parked tab is its home now", parked.ID)
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

// TestListTasks_QueueNext_MembershipAndOrdering pins PR-2's redefinition of
// "queue_next" (docs/plans/suggestion-as-state-transition-impl.md §4.1,
// design doc §3.6 "一覧は suggestion で駆動する"): membership is now
// `suggestion_verb != ”` regardless of status — the old state ∈ {ready,
// triaged} ∧ urgency ∈ {now,today,week} predicate (Phase 1 PR-3) is GONE.
// Any status (parked/working/done/dropped, even legacy ones) shows up here
// the moment it carries a suggestion; urgency drops from a membership gate
// to an ORDER BY tie-breaker only (a suggested card with no urgency at all
// still appears, just sorted last).
func TestListTasks_QueueNext_MembershipAndOrdering(t *testing.T) {
	d := createTestProject(t)

	mk := func(title string, status orchestrator.TaskStatus, verb, urgency string) *orchestrator.Task {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: title, Behavior: "dev", Status: status}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if verb != "" || urgency != "" {
			if err := orchestrator.UpsertTaskTriage(d.Conn, &orchestrator.TaskTriage{TaskID: task.ID, SuggestionVerb: verb, Urgency: urgency}); err != nil {
				t.Fatalf("upsert task_triage %s: %v", title, err)
			}
		}
		time.Sleep(2 * time.Millisecond) // force distinct created_at for ordering
		return task
	}

	// Created oldest-first, deliberately out of urgency order, so the
	// assertions below only pass if urgency-rank actually wins over
	// created_at ordering.
	nowOlder := mk("suggested-now-older", orchestrator.TaskStatusParked, "park", orchestrator.UrgencyNow)
	nowNewer := mk("suggested-now-newer", orchestrator.TaskStatusWorking, "done", orchestrator.UrgencyNow)
	today := mk("suggested-today", orchestrator.TaskStatusParked, "go", orchestrator.UrgencyToday)
	week := mk("suggested-week", orchestrator.TaskStatusParked, "drop", orchestrator.UrgencyWeek)
	noUrgency := mk("suggested-no-urgency", orchestrator.TaskStatusParked, "working", "")
	// A verb on a done/dropped card (khi suggesting reopen) is a real edge —
	// design doc §3.2's done/dropped→parked reopen edges — so it must appear
	// too, not just parked/working.
	doneReopen := mk("done-card-reopen-suggested", orchestrator.TaskStatusDone, "reopen", orchestrator.UrgencyNow)

	_ = mk("parked-no-suggestion", orchestrator.TaskStatusParked, "", orchestrator.UrgencyNow)
	_ = mk("working-no-suggestion", orchestrator.TaskStatusWorking, "", "")
	_ = mk("done-no-suggestion", orchestrator.TaskStatusDone, "", "")
	_ = mk("dropped-no-suggestion", orchestrator.TaskStatusDropped, "", "")

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "queue_next"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}

	var gotIDs []string
	for _, tk := range got {
		gotIDs = append(gotIDs, tk.ID)
	}
	// Ordering: now (oldest first) > today > week > no-urgency (someday/empty
	// sort last, same as UrgencyRank's fallback).
	want := []string{nowOlder.ID, nowNewer.ID, doneReopen.ID, today.ID, week.ID, noUrgency.ID}
	if len(gotIDs) != len(want) {
		t.Fatalf("queue_next returned %d tasks, want %d: got %v", len(gotIDs), len(want), gotIDs)
	}
	// nowOlder/nowNewer/doneReopen are all urgency=now, created in that
	// order — only their relative order (created_at ASC) matters among the
	// three; the block as a whole must sort before today/week/noUrgency.
	nowBlock := map[string]bool{nowOlder.ID: true, nowNewer.ID: true, doneReopen.ID: true}
	for i, id := range gotIDs[:3] {
		if !nowBlock[id] {
			t.Errorf("position %d: got %s, want one of the urgency=now tasks (order so far: %v)", i, id, gotIDs)
		}
	}
	if gotIDs[3] != today.ID || gotIDs[4] != week.ID || gotIDs[5] != noUrgency.ID {
		t.Errorf("tail ordering = %v, want [today, week, noUrgency] = [%s, %s, %s]", gotIDs[3:], today.ID, week.ID, noUrgency.ID)
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
