package orchestrator_test

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestChildCount_NonTerminalChildrenCountAsOpen verifies open_child_count
// counts every non-terminal child as open regardless of Type — card
// (parked/working) and execution (pending/executing) alike — and excludes
// only terminal statuses (done/aborted/dropped) alongside either type
// (Opus指摘#6: 逆輸入1で子は最初から task 化しないため、pre-execution な子が
// 親の done 主張を塞ぐ心配はそもそも発生しない — 緩めると verifyDoneClaim の
// 詐称防止ガードを無意味に弱めるだけになる)。
//
// card-model-cleanup PR-2: this used to enumerate the legacy pre-execution
// card statuses (captured/triaged/ready), which no longer exist. The
// underlying claim (taskChildCountCols' open_child_count is a pure
// terminal-status exclusion, type-agnostic) still holds, so this is
// rewritten against the current status vocabulary rather than deleted.
func TestChildCount_NonTerminalChildrenCountAsOpen(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Parent",
		Type:      orchestrator.TaskTypeExecution,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	openChildren := []*orchestrator.Task{
		{ProjectID: "proj-1", Title: "card-parked", ParentID: parent.ID, Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}},
		{ProjectID: "proj-1", Title: "card-working", ParentID: parent.ID, Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}},
		{ProjectID: "proj-1", Title: "exec-pending", ParentID: parent.ID, Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
		{ProjectID: "proj-1", Title: "exec-executing", ParentID: parent.ID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
	}
	terminalChildren := []*orchestrator.Task{
		{ProjectID: "proj-1", Title: "card-dropped", ParentID: parent.ID, Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusDropped, Card: &orchestrator.CardAttrs{}},
		{ProjectID: "proj-1", Title: "card-done", ParentID: parent.ID, Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusDone, Card: &orchestrator.CardAttrs{}},
		{ProjectID: "proj-1", Title: "exec-aborted", ParentID: parent.ID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
	}
	for _, c := range append(append([]*orchestrator.Task{}, openChildren...), terminalChildren...) {
		if err := orchestrator.CreateTask(d.Conn, c); err != nil {
			t.Fatalf("create child %s: %v", c.Title, err)
		}
	}

	got, err := orchestrator.GetTask(d.Conn, parent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OpenChildCount != len(openChildren) {
		t.Errorf("OpenChildCount = %d, want %d (terminal excluded, non-terminal counted regardless of type)", got.OpenChildCount, len(openChildren))
	}
}

// TestListTasks_Open_ExcludesBareParkedTask pins PR-2's review conclusion on
// notOpenSelfStatusSQLList's carve-out (docs/plans/
// suggestion-as-state-transition-impl.md §4.1's "Open タブに何が出るべきか"
// question): under card machine v2, EVERY new card lands directly in
// "parked" (task_resolve_or_capture.go / task_create.go, PR-1). A bare
// (childless) parked task must stay excluded from Open — the Parked tab
// (filters.templ) already owns it (design doc §3.6's "queue は唯一の窓ではない"
// requirement is met by the Parked tab, not by widening Open).
//
// card-model-cleanup PR-2: this test absorbs the old
// TestListTasks_Open_ExcludesBarePreExecutionTask, which pinned the exact
// same exclusion via the now-removed TaskStatusTriaged — under the current
// vocabulary "triaged" and "parked" are the same status (parked), so the two
// tests were fully redundant; this one also keeps the "ordinary pending task
// stays visible" half of that older test so no coverage is lost.
func TestListTasks_Open_ExcludesBareParkedTask(t *testing.T) {
	d := createTestProject(t)
	parked := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Parked, no children",
		Type:      orchestrator.TaskTypeCard,
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := orchestrator.CreateTask(d.Conn, parked); err != nil {
		t.Fatalf("create: %v", err)
	}
	pending := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Ordinary pending",
		Type:      orchestrator.TaskTypeExecution,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
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
	if ids[parked.ID] {
		t.Errorf("open view must not include a bare parked task (%s) — Parked tab is its home now", parked.ID)
	}
	if !ids[pending.ID] {
		t.Errorf("open view must still include ordinary pending tasks")
	}
}

// TestListTasks_Open_ExcludesChildlessPreExecutionTaskWithTerminalParent は
// codex レビューで見つかったバグの回帰テスト: 親 (done) を持つ childless な
// parked card が、祖先救済 CTE の base case が「自分自身」を含んでいたせいで
// 親の状態に関係なく open に漏れていた。CTE を「非 terminal な祖先の子孫のみ」
// に再構成して修正済み。
//
// card-model-cleanup PR-2: the fixture originally used the now-removed
// TaskStatusTriaged; parked is the current non-terminal card status this CTE
// regression applies to.
func TestListTasks_Open_ExcludesChildlessPreExecutionTaskWithTerminalParent(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Done parent",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusDone,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Parked, no children",
		Type:      orchestrator.TaskTypeCard,
		Status:    orchestrator.TaskStatusParked,
		ParentID:  parent.ID,
		Card:      &orchestrator.CardAttrs{},
	}
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

// card-model-cleanup PR-2: fixture's parent used to be TaskStatusTriaged
// (removed); parked is the current equivalent "set-aside, not yet decided"
// card status.
func TestListTasks_Open_IncludesPreExecutionParentWithExecutingChild(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Parked parent",
		Type:      orchestrator.TaskTypeCard,
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Dispatched child",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		ParentID:  parent.ID,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
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
		t.Error("open view must include a parked parent that has a live (executing) child — do not hide in-progress triage work")
	}
	if !ids[child.ID] {
		t.Error("open view must include the executing child itself")
	}
}

// card-model-cleanup PR-2: the third (excluded) fixture used to be
// TaskStatusTriaged (removed); parked is the current non-terminal card
// status that must still be excluded from "closed".
func TestListTasks_Closed_IncludesDropped(t *testing.T) {
	d := createTestProject(t)
	dropped := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Dropped",
		Type:      orchestrator.TaskTypeCard,
		Status:    orchestrator.TaskStatusDropped,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := orchestrator.CreateTask(d.Conn, dropped); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Done",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusDone,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, done); err != nil {
		t.Fatalf("create: %v", err)
	}
	parked := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Parked",
		Type:      orchestrator.TaskTypeCard,
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := orchestrator.CreateTask(d.Conn, parked); err != nil {
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
	if ids[parked.ID] {
		t.Error("closed view must not include a pre-execution (non-terminal) task")
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
//
// card-model-cleanup PR-2: the legacy captured/triaged/ready fixtures are
// gone (no longer valid statuses); working/done/dropped (the card statuses
// that used to risk a "superset" branch matching more than exact "parked")
// are kept.
func TestListTasks_Parked_ReturnsOnlyParkedTasks(t *testing.T) {
	d := createTestProject(t)
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
	}
	ids := map[orchestrator.TaskStatus]string{}
	for _, s := range statuses {
		task := &orchestrator.Task{
			ProjectID: "proj-1",
			Title:     "task-" + string(s),
			Type:      orchestrator.TaskTypeCard,
			Status:    s,
			Card:      &orchestrator.CardAttrs{},
		}
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
	greatGrandparent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "gg-parent (open)",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, greatGrandparent); err != nil {
		t.Fatalf("create great-grandparent: %v", err)
	}
	grandparent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "grandparent (done)",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusDone,
		ParentID:  greatGrandparent.ID,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, grandparent); err != nil {
		t.Fatalf("create grandparent: %v", err)
	}
	parent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "parent (done)",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusDone,
		ParentID:  grandparent.ID,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "child (done)",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusDone,
		ParentID:  parent.ID,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
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
	grandparent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "grandparent (aborted)",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusAborted,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, grandparent); err != nil {
		t.Fatalf("create grandparent: %v", err)
	}
	parent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "parent (done)",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusDone,
		ParentID:  grandparent.ID,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "child (done)",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusDone,
		ParentID:  parent.ID,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
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

// TestListTasks_Open_IncludesWorking pins down PR-2's deliberate
// classification of TaskStatusWorking as execution-like rather than
// pre-execution (see TaskStatusWorking's own doc comment in model.go): a
// childless working card must behave exactly like a childless executing task
// for filtering purposes — visible in "open" (unlike parked, which needs a
// live child to be rescued), invisible in "queue_next" unless it also
// carries a suggestion.
func TestListTasks_Open_IncludesWorking(t *testing.T) {
	d := createTestProject(t)
	working := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Working, no children",
		Type:      orchestrator.TaskTypeCard,
		Status:    orchestrator.TaskStatusWorking,
		Card:      &orchestrator.CardAttrs{},
	}
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
// `suggestion_verb != ""` regardless of status — the old state ∈ {ready,
// triaged} ∧ urgency ∈ {now,today,week} predicate (Phase 1 PR-3) is GONE.
// Any status (parked/working/done/dropped, even legacy ones) shows up here
// the moment it carries a suggestion; urgency drops from a membership gate
// to an ORDER BY tie-breaker only (a suggested card with no urgency at all
// still appears, just sorted last).
func TestListTasks_QueueNext_MembershipAndOrdering(t *testing.T) {
	d := createTestProject(t)

	mk := func(title string, status orchestrator.TaskStatus, verb, urgency string) *orchestrator.Task {
		task := &orchestrator.Task{
			ProjectID: "proj-1",
			Title:     title,
			Type:      orchestrator.TaskTypeCard,
			Status:    status,
			Card:      &orchestrator.CardAttrs{},
		}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if verb != "" || urgency != "" {
			if err := orchestrator.UpsertTaskTriage(d.Conn, &orchestrator.CardAttrs{TaskID: task.ID, SuggestionVerb: verb, Urgency: urgency}); err != nil {
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
	// Ordering: now (oldest-created first — the created_at ASC tiebreak
	// store.go's ORDER BY comment calls out as "古いものを腐らせない") > today >
	// week > no-urgency (someday/empty sort last, same as UrgencyRank's
	// fallback). PR #988 review, LOW 5: this must assert the EXACT position
	// of all six rows, not just set membership within the first three —
	// nowOlder/nowNewer/doneReopen were deliberately created oldest-first
	// specifically so a store.go regression that dropped "t.created_at ASC"
	// from the ORDER BY (leaving only urgency-rank, with whatever order
	// sqlite happens to return ties in) would go undetected by a
	// membership-only check.
	want := []string{nowOlder.ID, nowNewer.ID, doneReopen.ID, today.ID, week.ID, noUrgency.ID}
	if len(gotIDs) != len(want) {
		t.Fatalf("queue_next returned %d tasks, want %d: got %v", len(gotIDs), len(want), gotIDs)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("position %d: got task id %s, want %s (full order: got=%v want=%v)", i, gotIDs[i], want[i], gotIDs, want)
		}
	}
}

// TestListTasks_Triage_ReturnsPreExecutionPlusWorking pins the "triage"
// filter's current predicate (store.go): `t.type = 'card' AND t.status IN
// ('parked', 'working')`. card-model-cleanup PR-2 restated this directly on
// tasks.type instead of enumerating a legacy pre-execution status set
// (captured/triaged/ready, all removed) — so this is now a TWO-axis pin, not
// just a status pin: it must exclude both non-matching card statuses
// (done/dropped) AND every execution task regardless of status, since
// "triage" only ever means "a live card".
func TestListTasks_Triage_ReturnsPreExecutionPlusWorking(t *testing.T) {
	d := createTestProject(t)
	type fixture struct {
		task     *orchestrator.Task
		included bool
	}
	fixtures := []fixture{
		{&orchestrator.Task{ProjectID: "proj-1", Title: "card-parked", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}, true},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "card-working", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}, true},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "card-done", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusDone, Card: &orchestrator.CardAttrs{}}, false},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "card-dropped", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusDropped, Card: &orchestrator.CardAttrs{}}, false},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "exec-pending", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}, false},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "exec-executing", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}, false},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "exec-awaiting", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusAwaiting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}, false},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "exec-done", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}, false},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "exec-aborted", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}, false},
	}
	for _, f := range fixtures {
		if err := orchestrator.CreateTask(d.Conn, f.task); err != nil {
			t.Fatalf("create %s: %v", f.task.Title, err)
		}
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "triage"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, tk := range got {
		gotIDs[tk.ID] = true
	}
	for _, f := range fixtures {
		if f.included && !gotIDs[f.task.ID] {
			t.Errorf("%s (type=%s, status=%s) missing from the triage filter", f.task.Title, f.task.Type, f.task.Status)
		}
		if !f.included && gotIDs[f.task.ID] {
			t.Errorf("%s (type=%s, status=%s) must not appear in the triage filter", f.task.Title, f.task.Type, f.task.Status)
		}
	}
}
