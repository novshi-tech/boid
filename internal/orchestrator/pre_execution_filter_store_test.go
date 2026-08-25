package orchestrator_test

import (
	"testing"

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

// TestListTasks_Open_NoLongerRescuesGreatGrandchild pins the PR-4 narrowing
// (docs/plans/webui-detail-list-redesign.md §3.6): the multi-level
// open_descendants/open_ancestors recursive CTEs are DELETED — they existed
// only to keep the list-page tree (task_tree.templ) from showing an orphaned
// row, and that tree is gone. "open" now rescues only DIRECT (1-level)
// children of a non-terminal self (still exercised by
// TestListTasks_Open_IncludesPreExecutionParentWithExecutingChild below); a
// grandchild several levels down a chain of terminal intermediates is no
// longer rescued. This is the direct negative twin of the old
// TestListTasks_Open_RescuesGreatGrandchildOfOpenAncestor this test replaces.
func TestListTasks_Open_NoLongerRescuesGreatGrandchild(t *testing.T) {
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
	// greatGrandparent itself is still open (self clause: executing).
	if !ids[greatGrandparent.ID] {
		t.Error("open view must still include the executing great-grandparent itself")
	}
	// grandparent/parent/child are all terminal (done) with no live child of
	// their own — none of them are rescued by the (now 1-level-only) child
	// rescue, and the removed multi-level CTEs no longer reach past them.
	for _, tk := range []*orchestrator.Task{grandparent, parent, child} {
		if ids[tk.ID] {
			t.Errorf("open view must NOT include %q (%s) now that multi-level ancestor/descendant rescue is removed (PR-4)", tk.Title, tk.ID)
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

// TestListTasks_QueueNext_ReturnsEmpty pins docs/plans/
// webui-detail-list-redesign.md PR-4's decided answer to 論点3: explicitly
// requesting Status: "queue_next" (the pre-PR-4 Queue tab's membership
// predicate, `suggestion_verb != ”`) now returns an EMPTY list — not an
// error, not the suggestion-bearing cards it used to match — because
// "queue_next" is no longer a special-cased predicate at all, just a literal
// status string that can never match a real tasks.status value. Fixtures
// deliberately carry live suggestions so a regression that silently restored
// the old predicate (instead of truly deleting it) would fail this test by
// returning non-empty.
func TestListTasks_QueueNext_ReturnsEmpty(t *testing.T) {
	d := createTestProject(t)
	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "suggested",
		Type:      orchestrator.TaskTypeCard,
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.UpsertTaskTriage(d.Conn, &orchestrator.CardAttrs{TaskID: task.ID, SuggestionVerb: "go", Urgency: "now"}); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "queue_next"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListTasks(Status: %q) = %d tasks, want 0 (queue_next predicate removed, PR-4)", "queue_next", len(got))
	}
}

// TestListTasks_CardsLive_ReturnsPreExecutionPlusWorking pins the
// "cards_live" filter's current predicate (store.go, renamed from "triage"
// by docs/plans/webui-detail-list-redesign.md PR-4 — §3.6 makes clear the
// PREDICATE is unchanged, only the name is): `t.type = 'card' AND t.status
// IN ('parked', 'working')`. card-model-cleanup PR-2 restated this directly
// on tasks.type instead of enumerating a legacy pre-execution status set
// (captured/triaged/ready, all removed) — so this is now a TWO-axis pin, not
// just a status pin: it must exclude both non-matching card statuses
// (done/dropped) AND every execution task regardless of status, since
// "cards_live" only ever means "a live card".
func TestListTasks_CardsLive_ReturnsPreExecutionPlusWorking(t *testing.T) {
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

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "cards_live"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, tk := range got {
		gotIDs[tk.ID] = true
	}
	for _, f := range fixtures {
		if f.included && !gotIDs[f.task.ID] {
			t.Errorf("%s (type=%s, status=%s) missing from the cards_live filter", f.task.Title, f.task.Type, f.task.Status)
		}
		if !f.included && gotIDs[f.task.ID] {
			t.Errorf("%s (type=%s, status=%s) must not appear in the cards_live filter", f.task.Title, f.task.Type, f.task.Status)
		}
	}
}

// TestListTasks_TriageAlias_MatchesCardsLive pins the PR-4 backward-compat
// promise (§3.6): the old Status: "triage" string, if an explicit caller
// still sends it, must return EXACTLY the same set as the canonical
// "cards_live" — a stale bookmark, script, or `boid card list --status
// triage` invocation keeps working, not just "doesn't error".
func TestListTasks_TriageAlias_MatchesCardsLive(t *testing.T) {
	d := createTestProject(t)
	fixtures := []*orchestrator.Task{
		{ProjectID: "proj-1", Title: "card-parked", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}},
		{ProjectID: "proj-1", Title: "card-working", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}},
		{ProjectID: "proj-1", Title: "card-done", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusDone, Card: &orchestrator.CardAttrs{}},
	}
	for _, f := range fixtures {
		if err := orchestrator.CreateTask(d.Conn, f); err != nil {
			t.Fatalf("create %s: %v", f.Title, err)
		}
	}

	viaCanonical, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "cards_live"})
	if err != nil {
		t.Fatalf("ListTasks(cards_live): %v", err)
	}
	viaAlias, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "triage"})
	if err != nil {
		t.Fatalf("ListTasks(triage): %v", err)
	}
	toIDs := func(tasks []*orchestrator.Task) map[string]bool {
		out := map[string]bool{}
		for _, t := range tasks {
			out[t.ID] = true
		}
		return out
	}
	canonicalIDs, aliasIDs := toIDs(viaCanonical), toIDs(viaAlias)
	if len(canonicalIDs) == 0 {
		t.Fatal("fixture bug: cards_live returned no rows")
	}
	if len(canonicalIDs) != len(aliasIDs) {
		t.Fatalf("cards_live returned %d rows, triage alias returned %d — want equal", len(canonicalIDs), len(aliasIDs))
	}
	for id := range canonicalIDs {
		if !aliasIDs[id] {
			t.Errorf("triage alias missing task %s that cards_live included", id)
		}
	}
}
