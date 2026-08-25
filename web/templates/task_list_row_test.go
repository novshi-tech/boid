package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- triageSummary / decodeSummaryString ---
//
// Moved verbatim from the pre-PR-4 internal/api/tree_summary_test.go
// (deleted by PR-4, docs/plans/webui-detail-list-redesign.md §3.5) — the
// function itself moved to task_list_row.templ unchanged, only its test
// coverage went missing in the move. See [[next-session-webui-detail-list-impl]]
// (follow-up 2) in memory.
//
// triageSummary backs list row 3's card-summary fallback. The only writer a
// workspace has for task_triage.detail is the attrs_set action, and that
// folds into detail.attrs (FoldDetailAttrs, internal/orchestrator/card.go) —
// never the top level. So a reader that looks at detail.summary alone can
// never see anything a workspace wrote, and the badge stays empty forever.
//
// The fallback mirrors DetailSuggestion (card.go), which already reads
// top-level first and falls back to attrs for exactly the same reason.

func TestTriageSummary_TopLevel(t *testing.T) {
	got := triageSummary([]byte(`{"summary":"top"}`))
	if got != "top" {
		t.Fatalf("summary = %q, want %q", got, "top")
	}
}

func TestTriageSummary_FallsBackToAttrs(t *testing.T) {
	// What a workspace can actually produce: attrs_set{"summary": ...}.
	got := triageSummary([]byte(`{"attrs":{"summary":"from attrs","urgency":"now"}}`))
	if got != "from attrs" {
		t.Fatalf("summary = %q, want %q", got, "from attrs")
	}
}

func TestTriageSummary_TopLevelWinsOverAttrs(t *testing.T) {
	// A future writer that places it at the top level should not be shadowed by
	// a stale attrs copy.
	got := triageSummary([]byte(`{"summary":"top","attrs":{"summary":"attrs"}}`))
	if got != "top" {
		t.Fatalf("summary = %q, want %q", got, "top")
	}
}

func TestTriageSummary_EmptyTopLevelDoesNotBlockTheFallback(t *testing.T) {
	got := triageSummary([]byte(`{"summary":"","attrs":{"summary":"attrs"}}`))
	if got != "attrs" {
		t.Fatalf("summary = %q, want %q", got, "attrs")
	}
}

func TestTriageSummary_Absent(t *testing.T) {
	for _, detail := range []string{``, `null`, `{}`, `{"attrs":{}}`, `{"children":[]}`} {
		if got := triageSummary([]byte(detail)); got != "" {
			t.Errorf("triageSummary(%s) = %q, want empty", detail, got)
		}
	}
}

// Never panics, never partially-fabricates a value — same best-effort posture
// the function already had: a malformed blob must not sink the row it lives on.
func TestTriageSummary_Malformed(t *testing.T) {
	for _, detail := range []string{
		`not json`,
		`{"summary":`,
		`{"summary":123}`,             // wrong type at the top level
		`{"attrs":"not an object"}`,   // attrs is not a map
		`{"attrs":{"summary":["a"]}}`, // wrong type inside attrs
	} {
		if got := triageSummary([]byte(detail)); got != "" {
			t.Errorf("triageSummary(%s) = %q, want empty", detail, got)
		}
	}
}

// A wrong-typed value in one location must not blank out a well-formed value in
// the other (the finding that shaped decodeSuggestion's own shape: Opus review,
// 2026-08-18).
func TestTriageSummary_BadTopLevelStillReadsAttrs(t *testing.T) {
	got := triageSummary([]byte(`{"summary":123,"attrs":{"summary":"attrs"}}`))
	if got != "attrs" {
		t.Fatalf("summary = %q, want %q", got, "attrs")
	}
}

// --- childRollupLabel ---
//
// Pins §3.5's row-2 child rollup shape (task_list_row.templ's own doc
// comment): "子 N" always when N>0, "進行 M"/"完了 M" mutually exclusive,
// "⚠ K" appended regardless. Previously untested — a mutation that always
// returns "" passed `go test ./web/...` green (memory: [[next-session-
// webui-detail-list-redesign.md]] follow-up 1, N1).

func TestChildRollupLabel_NoChildren_ReturnsEmpty(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 0}
	if got := childRollupLabel(task); got != "" {
		t.Errorf("childRollupLabel(no children) = %q, want empty", got)
	}
}

func TestChildRollupLabel_InProgress(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 3, OpenChildCount: 2}
	want := "子 3 · 進行 2"
	if got := childRollupLabel(task); got != want {
		t.Errorf("childRollupLabel = %q, want %q", got, want)
	}
}

func TestChildRollupLabel_AllDoneNoneInProgress(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 2, OpenChildCount: 0, DoneChildCount: 2}
	want := "子 2 · 完了 2"
	if got := childRollupLabel(task); got != want {
		t.Errorf("childRollupLabel = %q, want %q", got, want)
	}
}

func TestChildRollupLabel_NoneInProgressNoneDone_OnlyCount(t *testing.T) {
	// e.g. every child aborted: no in-progress, nothing done either — just the
	// bare count, no 進行/完了 segment.
	task := &orchestrator.Task{TotalChildCount: 1, OpenChildCount: 0, DoneChildCount: 0, AbortedChildCount: 1}
	want := "子 1"
	if got := childRollupLabel(task); got != want {
		t.Errorf("childRollupLabel = %q, want %q", got, want)
	}
}

// §2.4's gap this rollup closes: awaiting is always surfaced, on top of
// whichever progress/done segment applies.
func TestChildRollupLabel_AwaitingAppendedOnTopOfInProgress(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 4, OpenChildCount: 3, AwaitingChildCount: 1}
	want := "子 4 · 進行 2 · ⚠ 1"
	if got := childRollupLabel(task); got != want {
		t.Errorf("childRollupLabel = %q, want %q", got, want)
	}
}

func TestChildRollupLabel_AwaitingAppendedOnTopOfDone(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 2, OpenChildCount: 1, AwaitingChildCount: 1, DoneChildCount: 1}
	// inProgress = OpenChildCount - AwaitingChildCount = 0, so it falls to the
	// done branch even though OpenChildCount > 0.
	want := "子 2 · 完了 1 · ⚠ 1"
	if got := childRollupLabel(task); got != want {
		t.Errorf("childRollupLabel = %q, want %q", got, want)
	}
}

// Defense-in-depth: OpenChildCount should never be less than
// AwaitingChildCount in practice, but the function clamps rather than
// rendering a negative "進行 -1".
func TestChildRollupLabel_InProgressNeverNegative(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 1, OpenChildCount: 0, AwaitingChildCount: 1}
	got := childRollupLabel(task)
	if strings.Contains(got, "進行 -") {
		t.Errorf("childRollupLabel must not render a negative 進行 count, got %q", got)
	}
	want := "子 1 · ⚠ 1"
	if got != want {
		t.Errorf("childRollupLabel = %q, want %q", got, want)
	}
}

// --- relativeTimeLabel ---

func TestRelativeTimeLabel_JustNow(t *testing.T) {
	if got := relativeTimeLabel(time.Now().Add(-10 * time.Second)); got != "たった今" {
		t.Errorf("relativeTimeLabel(10s ago) = %q, want たった今", got)
	}
}

func TestRelativeTimeLabel_Minutes(t *testing.T) {
	if got := relativeTimeLabel(time.Now().Add(-5 * time.Minute)); got != "5m" {
		t.Errorf("relativeTimeLabel(5m ago) = %q, want 5m", got)
	}
}

func TestRelativeTimeLabel_Hours(t *testing.T) {
	if got := relativeTimeLabel(time.Now().Add(-3 * time.Hour)); got != "3h" {
		t.Errorf("relativeTimeLabel(3h ago) = %q, want 3h", got)
	}
}

func TestRelativeTimeLabel_Days(t *testing.T) {
	if got := relativeTimeLabel(time.Now().Add(-2 * 24 * time.Hour)); got != "2d" {
		t.Errorf("relativeTimeLabel(2d ago) = %q, want 2d", got)
	}
}

// A future timestamp (clock skew, or a task whose UpdatedAt was just set)
// must not render a negative duration.
func TestRelativeTimeLabel_FutureClampedToZero(t *testing.T) {
	if got := relativeTimeLabel(time.Now().Add(1 * time.Hour)); got != "たった今" {
		t.Errorf("relativeTimeLabel(future) = %q, want たった今 (clamped)", got)
	}
}

// --- ListRow.ProjectLabel ---

func TestListRow_ProjectLabel_UsesResolvedName(t *testing.T) {
	row := ListRow{Task: &orchestrator.Task{ProjectID: "proj-1"}, ProjectName: "My Project"}
	if got := row.ProjectLabel(); got != "My Project" {
		t.Errorf("ProjectLabel() = %q, want My Project", got)
	}
}

func TestListRow_ProjectLabel_FallsBackToProjectID(t *testing.T) {
	row := ListRow{Task: &orchestrator.Task{ProjectID: "proj-1"}}
	if got := row.ProjectLabel(); got != "proj-1" {
		t.Errorf("ProjectLabel() = %q, want proj-1 (fallback)", got)
	}
}

// --- BuildListRows ---
//
// Pins the two per-row triage enrichments (Suggestion, Summary) BuildListRows
// is responsible for attaching. Previously untested — mutations that deleted
// either populate call passed `go test ./web/...` green (memory follow-up 1,
// N2/N3).

func TestBuildListRows_PopulatesSuggestionFromTriage(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1"},
	}
	triage := map[string]*orchestrator.CardAttrs{
		"t-1": {Detail: []byte(`{"suggestion":{"verb":"go","reason":"ready"}}`)},
	}

	rows := BuildListRows(tasks, nil, triage)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Suggestion.Verb != "go" || rows[0].Suggestion.Reason != "ready" {
		t.Errorf("Suggestion = %+v, want Verb=go Reason=ready", rows[0].Suggestion)
	}
}

func TestBuildListRows_PopulatesSummaryFromTriage(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1"},
	}
	triage := map[string]*orchestrator.CardAttrs{
		"t-1": {Detail: []byte(`{"summary":"waiting on review"}`)},
	}

	rows := BuildListRows(tasks, nil, triage)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Summary != "waiting on review" {
		t.Errorf("Summary = %q, want %q", rows[0].Summary, "waiting on review")
	}
}

func TestBuildListRows_NoTriageRow_ZeroValues(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1"},
	}

	rows := BuildListRows(tasks, nil, map[string]*orchestrator.CardAttrs{})
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Suggestion.Verb != "" || rows[0].Summary != "" {
		t.Errorf("row without a triage entry should be zero-valued, got Suggestion=%+v Summary=%q", rows[0].Suggestion, rows[0].Summary)
	}
}

func TestBuildListRows_NilTriageMap_ZeroValues(t *testing.T) {
	tasks := []*orchestrator.Task{{ID: "t-1", ProjectID: "proj-1"}}
	rows := BuildListRows(tasks, nil, nil)
	if len(rows) != 1 || rows[0].Suggestion.Verb != "" || rows[0].Summary != "" {
		t.Errorf("nil triage map should not populate anything, got rows=%+v", rows)
	}
}

func TestBuildListRows_ResolvesProjectName(t *testing.T) {
	tasks := []*orchestrator.Task{{ID: "t-1", ProjectID: "proj-1"}}
	names := map[string]string{"proj-1": "My Project"}

	rows := BuildListRows(tasks, names, nil)
	if rows[0].ProjectName != "My Project" {
		t.Errorf("ProjectName = %q, want My Project", rows[0].ProjectName)
	}
}

// Input order must be preserved untouched — store.go's ListTasks already
// applies the view's ORDER BY, so BuildListRows must not re-sort.
func TestBuildListRows_PreservesInputOrder(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t-3"}, {ID: "t-1"}, {ID: "t-2"},
	}
	rows := BuildListRows(tasks, nil, nil)
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	wantOrder := []string{"t-3", "t-1", "t-2"}
	for i, want := range wantOrder {
		if rows[i].Task.ID != want {
			t.Errorf("rows[%d].Task.ID = %q, want %q", i, rows[i].Task.ID, want)
		}
	}
}

// --- render-level pins: the rollup and the summary fallback must actually
// reach the HTML, not just the Go struct. ---

func TestTaskListRow_ChildRollup_RendersInLine2(t *testing.T) {
	row := ListRow{Task: &orchestrator.Task{
		ID: "t-1", Title: "parent task", Status: orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{}, TotalChildCount: 3, OpenChildCount: 2,
	}}

	var buf bytes.Buffer
	if err := taskListRow(row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "子 3") || !strings.Contains(html, "進行 2") {
		t.Errorf("expected the child rollup in the rendered row, got: %s", html)
	}
}

func TestTaskListRow_NoChildren_RendersNoRollup(t *testing.T) {
	row := ListRow{Task: &orchestrator.Task{
		ID: "t-1", Title: "childless task", Status: orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{},
	}}

	var buf bytes.Buffer
	if err := taskListRow(row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := buf.String(); strings.Contains(html, "list-row-rollup") {
		t.Errorf("childless task should not render a rollup segment, got: %s", html)
	}
}

// §5 論点6: a card with no live suggestion falls back to khi's summary attr.
func TestTaskListRowMovement_CardWithNoSuggestion_FallsBackToSummary(t *testing.T) {
	row := ListRow{
		Task:    &orchestrator.Task{ID: "t-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked},
		Summary: "waiting on upstream review",
	}

	var buf bytes.Buffer
	if err := taskListRowMovement(row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := buf.String(); !strings.Contains(html, "waiting on upstream review") {
		t.Errorf("expected the summary fallback text, got: %s", html)
	}
}

func TestTaskListRowMovement_CardWithSuggestion_RendersVerbOverSummary(t *testing.T) {
	row := ListRow{
		Task:       &orchestrator.Task{ID: "t-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked},
		Suggestion: orchestrator.Suggestion{Verb: "go", Reason: "children specced"},
		Summary:    "stale summary that should not show",
	}

	var buf bytes.Buffer
	if err := taskListRowMovement(row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "children specced") {
		t.Errorf("expected the live suggestion's reason, got: %s", html)
	}
	if strings.Contains(html, "stale summary that should not show") {
		t.Errorf("a live suggestion must take priority over the summary fallback, got: %s", html)
	}
}
