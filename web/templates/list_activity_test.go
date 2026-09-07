package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- ActiveChildFromDetail ---

func TestActiveChildFromDetail_ReturnsFirstNonClosed(t *testing.T) {
	detail := []byte(`{"children":[
		{"id":"c1","status":"closed"},
		{"id":"c2","status":"specced"},
		{"id":"c3","status":"open"}
	]}`)
	got := ActiveChildFromDetail(detail)
	if got == nil || got.ID != "c2" {
		t.Fatalf("ActiveChildFromDetail = %+v, want c2 (first non-closed)", got)
	}
}

func TestActiveChildFromDetail_AllClosed_ReturnsNil(t *testing.T) {
	detail := []byte(`{"children":[{"id":"c1","status":"closed"}]}`)
	if got := ActiveChildFromDetail(detail); got != nil {
		t.Errorf("ActiveChildFromDetail = %+v, want nil", got)
	}
}

func TestActiveChildFromDetail_NoChildren_ReturnsNil(t *testing.T) {
	if got := ActiveChildFromDetail(nil); got != nil {
		t.Errorf("ActiveChildFromDetail(nil) = %+v, want nil", got)
	}
	if got := ActiveChildFromDetail([]byte(`{}`)); got != nil {
		t.Errorf("ActiveChildFromDetail({}) = %+v, want nil", got)
	}
}

func TestActiveChildFromDetail_Malformed_ReturnsNilNotPanic(t *testing.T) {
	if got := ActiveChildFromDetail([]byte(`not json`)); got != nil {
		t.Errorf("ActiveChildFromDetail(malformed) = %+v, want nil", got)
	}
}

// --- WorkActivityLabel ---
//
// Pins §5.5's work-child vocabulary table exactly, per input condition —
// see docs/plans/card-next-step-and-timeline.md §10 (PR-5c) for the
// confirmed table this mirrors.

func TestWorkActivityLabel_NoActiveChild_Empty(t *testing.T) {
	if got := WorkActivityLabel(nil, nil); got != "" {
		t.Errorf("WorkActivityLabel(nil) = %q, want empty", got)
	}
}

func TestWorkActivityLabel_Open_Draft(t *testing.T) {
	child := &orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusOpen}
	if got := WorkActivityLabel(child, nil); got != "Draft" {
		t.Errorf("WorkActivityLabel(open) = %q, want Draft", got)
	}
}

func TestWorkActivityLabel_Specced_ReadyToRun(t *testing.T) {
	child := &orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusSpecced}
	if got := WorkActivityLabel(child, nil); got != "Ready to run" {
		t.Errorf("WorkActivityLabel(specced) = %q, want %q", got, "Ready to run")
	}
}

func TestWorkActivityLabel_DispatchedPending_Queued(t *testing.T) {
	child := &orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-1"}
	statuses := map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusPending}
	if got := WorkActivityLabel(child, statuses); got != "Queued" {
		t.Errorf("WorkActivityLabel(dispatched/pending) = %q, want Queued", got)
	}
}

func TestWorkActivityLabel_DispatchedExecuting_Running(t *testing.T) {
	child := &orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-1"}
	statuses := map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusExecuting}
	if got := WorkActivityLabel(child, statuses); got != "Running" {
		t.Errorf("WorkActivityLabel(dispatched/executing) = %q, want Running", got)
	}
}

func TestWorkActivityLabel_DispatchedAwaiting_NeedsInput(t *testing.T) {
	child := &orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-1"}
	statuses := map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusAwaiting}
	if got := WorkActivityLabel(child, statuses); got != "Needs input" {
		t.Errorf("WorkActivityLabel(dispatched/awaiting) = %q, want %q", got, "Needs input")
	}
}

// The real task is authoritative — a dispatched child whose task has
// already reached a terminal status (a narrow, transient inconsistency
// before the child's own JSON status catches up) must NOT be reported as
// Running just because the JSON still says "dispatched" (§5.5: "子の状態は
// 実 task を正とし、JSON の dispatched だけで Running と断定しない").
func TestWorkActivityLabel_DispatchedButTaskTerminal_Empty(t *testing.T) {
	child := &orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-1"}
	statuses := map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusDone}
	if got := WorkActivityLabel(child, statuses); got != "" {
		t.Errorf("WorkActivityLabel(dispatched/done) = %q, want empty", got)
	}
}

func TestWorkActivityLabel_DispatchedButTaskMissing_Empty(t *testing.T) {
	child := &orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-missing"}
	if got := WorkActivityLabel(child, map[string]orchestrator.TaskStatus{}); got != "" {
		t.Errorf("WorkActivityLabel(dispatched/missing task) = %q, want empty", got)
	}
}

// --- CommandActivityLabel ---

func TestCommandActivityLabel_Nil_Empty(t *testing.T) {
	if got := CommandActivityLabel(nil, nil); got != "" {
		t.Errorf("CommandActivityLabel(nil) = %q, want empty", got)
	}
}

// launched_label is empty for a queued (never-launched) row — the fallback
// is the raw command_key, not a guessed verb.
func TestCommandActivityLabel_Queued_FallsBackToCommandKey(t *testing.T) {
	req := &orchestrator.CardRequest{CommandKey: "sweep", Status: orchestrator.CardRequestStatusQueued}
	if got := CommandActivityLabel(req, nil); got != "sweep: Queued" {
		t.Errorf("CommandActivityLabel(queued) = %q, want %q", got, "sweep: Queued")
	}
}

func TestCommandActivityLabel_Launching_UsesLaunchedLabel(t *testing.T) {
	req := &orchestrator.CardRequest{
		CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching,
		Launched: orchestrator.CardRequestDefinition{Label: "Discuss"},
	}
	if got := CommandActivityLabel(req, nil); got != "Discuss: Launching" {
		t.Errorf("CommandActivityLabel(launching) = %q, want %q", got, "Discuss: Launching")
	}
}

// Attached + a session target: no real status to check (session liveness is
// a job-table concern out of scope here), so the label stays "Running".
func TestCommandActivityLabel_AttachedSessionTarget_Running(t *testing.T) {
	req := &orchestrator.CardRequest{
		CommandKey: "discuss", Status: orchestrator.CardRequestStatusAttached,
		Launched:   orchestrator.CardRequestDefinition{Label: "Discuss"},
		TargetKind: orchestrator.CardRequestTargetKindSession, TargetID: "job-1",
	}
	if got := CommandActivityLabel(req, nil); got != "Discuss: Running" {
		t.Errorf("CommandActivityLabel(attached/session) = %q, want %q", got, "Discuss: Running")
	}
}

// Attached + a task target whose real task is awaiting: the command must
// say "Needs input", not "Running" — otherwise a user's answer-pending
// dialogue task is invisible in the list, the exact misreading §5.5 warns
// against for the work-child axis.
func TestCommandActivityLabel_AttachedTaskTarget_Awaiting_NeedsInput(t *testing.T) {
	req := &orchestrator.CardRequest{
		CommandKey: "discuss", Status: orchestrator.CardRequestStatusAttached,
		Launched:   orchestrator.CardRequestDefinition{Label: "Discuss"},
		TargetKind: orchestrator.CardRequestTargetKindTask, TargetID: "task-1",
	}
	statuses := map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusAwaiting}
	if got := CommandActivityLabel(req, statuses); got != "Discuss: Needs input" {
		t.Errorf("CommandActivityLabel(attached/task/awaiting) = %q, want %q", got, "Discuss: Needs input")
	}
}

func TestCommandActivityLabel_AttachedTaskTarget_Pending_Queued(t *testing.T) {
	req := &orchestrator.CardRequest{
		CommandKey: "discuss", Status: orchestrator.CardRequestStatusAttached,
		Launched:   orchestrator.CardRequestDefinition{Label: "Discuss"},
		TargetKind: orchestrator.CardRequestTargetKindTask, TargetID: "task-1",
	}
	statuses := map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusPending}
	if got := CommandActivityLabel(req, statuses); got != "Discuss: Queued" {
		t.Errorf("CommandActivityLabel(attached/task/pending) = %q, want %q", got, "Discuss: Queued")
	}
}

func TestCommandActivityLabel_AttachedTaskTarget_Executing_Running(t *testing.T) {
	req := &orchestrator.CardRequest{
		CommandKey: "discuss", Status: orchestrator.CardRequestStatusAttached,
		Launched:   orchestrator.CardRequestDefinition{Label: "Discuss"},
		TargetKind: orchestrator.CardRequestTargetKindTask, TargetID: "task-1",
	}
	statuses := map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusExecuting}
	if got := CommandActivityLabel(req, statuses); got != "Discuss: Running" {
		t.Errorf("CommandActivityLabel(attached/task/executing) = %q, want %q", got, "Discuss: Running")
	}
}

// A task target whose status is not (yet) in the batch map — a narrow,
// transient window before self-recording lands — must not disappear the
// badge; it falls back to "Running" rather than going blank.
// A task target that is terminal, or whose row is already gone to GC, must
// say nothing rather than claim the command is still running — the same
// call the work-child axis makes for a dispatched child whose task ended.
func TestCommandActivityLabel_AttachedTaskTarget_TerminalOrMissing_Empty(t *testing.T) {
	req := &orchestrator.CardRequest{
		CommandKey: "discuss", Status: orchestrator.CardRequestStatusAttached,
		Launched:   orchestrator.CardRequestDefinition{Label: "Discuss"},
		TargetKind: orchestrator.CardRequestTargetKindTask, TargetID: "task-1",
	}
	for _, tc := range []struct {
		name     string
		statuses map[string]orchestrator.TaskStatus
	}{
		{"missing (GC'd)", map[string]orchestrator.TaskStatus{}},
		{"done", map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusDone}},
		{"aborted", map[string]orchestrator.TaskStatus{"task-1": orchestrator.TaskStatusAborted}},
	} {
		if got := CommandActivityLabel(req, tc.statuses); got != "" {
			t.Errorf("CommandActivityLabel(attached/task/%s) = %q, want empty", tc.name, got)
		}
	}
}

// The two axes must map a live task's status through one shared rule, so
// renaming a word on one axis cannot silently leave the other behind.
func TestWorkAndCommandAxes_ShareTheSameLiveTaskWords(t *testing.T) {
	for _, tc := range []struct {
		status orchestrator.TaskStatus
		want   string
	}{
		{orchestrator.TaskStatusPending, "Queued"},
		{orchestrator.TaskStatusExecuting, "Running"},
		{orchestrator.TaskStatusAwaiting, "Needs input"},
		{orchestrator.TaskStatusDone, ""},
	} {
		statuses := map[string]orchestrator.TaskStatus{"t1": tc.status}
		child := &orchestrator.TaskTriageChild{
			Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "t1",
		}
		work := WorkActivityLabel(child, statuses)
		req := &orchestrator.CardRequest{
			CommandKey: "discuss", Status: orchestrator.CardRequestStatusAttached,
			Launched:   orchestrator.CardRequestDefinition{Label: "Discuss"},
			TargetKind: orchestrator.CardRequestTargetKindTask, TargetID: "t1",
		}
		command := CommandActivityLabel(req, statuses)
		wantCommand := ""
		if tc.want != "" {
			wantCommand = "Discuss: " + tc.want
		}
		if work != tc.want || command != wantCommand {
			t.Errorf("status %q: work = %q (want %q), command = %q (want %q)",
				tc.status, work, tc.want, command, wantCommand)
		}
	}
}

func TestCommandActivityLabel_TerminalStatus_Empty(t *testing.T) {
	req := &orchestrator.CardRequest{CommandKey: "discuss", Status: orchestrator.CardRequestStatusFinished}
	if got := CommandActivityLabel(req, nil); got != "" {
		t.Errorf("CommandActivityLabel(finished) = %q, want empty", got)
	}
}

// --- BuildCardActivityStates ---

func TestBuildCardActivityStates_CombinesBothAxesIndependently(t *testing.T) {
	activeChildren := map[string]*orchestrator.TaskTriageChild{
		"card-1": {ID: "c1", Status: orchestrator.TaskTriageChildStatusSpecced},
	}
	activeRequests := map[string]*orchestrator.CardRequest{
		"card-1": {CommandKey: "discuss", Status: orchestrator.CardRequestStatusAttached, Launched: orchestrator.CardRequestDefinition{Label: "Discuss"}},
	}
	got := BuildCardActivityStates([]string{"card-1"}, activeChildren, nil, activeRequests)
	state, ok := got["card-1"]
	if !ok {
		t.Fatalf("card-1 missing from %+v", got)
	}
	if state.WorkLabel != "Ready to run" {
		t.Errorf("WorkLabel = %q, want %q", state.WorkLabel, "Ready to run")
	}
	if state.CommandLabel != "Discuss: Running" {
		t.Errorf("CommandLabel = %q, want %q", state.CommandLabel, "Discuss: Running")
	}
}

func TestBuildCardActivityStates_NeitherAxisActive_CardAbsentFromResult(t *testing.T) {
	got := BuildCardActivityStates([]string{"card-1"}, nil, nil, nil)
	if _, ok := got["card-1"]; ok {
		t.Errorf("card-1 present with no active work child or command: %+v", got)
	}
}

// --- render-level pin: the activity badges must actually reach the HTML ---

func TestTaskListRowMovement_CardActivity_RendersBothBadges(t *testing.T) {
	row := ListRow{
		Task:     &orchestrator.Task{ID: "t-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking},
		Activity: CardActivityState{WorkLabel: "Running", CommandLabel: "Discuss: Launching"},
	}
	var buf bytes.Buffer
	if err := taskListRowMovement(row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "Running") {
		t.Errorf("expected the work activity label, got: %s", html)
	}
	if !strings.Contains(html, "Discuss: Launching") {
		t.Errorf("expected the command activity label, got: %s", html)
	}
}

func TestTaskListRowMovement_ExecTask_NeverRendersActivityBadges(t *testing.T) {
	row := ListRow{
		Task:     &orchestrator.Task{ID: "t-1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}},
		Activity: CardActivityState{WorkLabel: "Running", CommandLabel: "Discuss: Launching"},
	}
	var buf bytes.Buffer
	if err := taskListRowMovement(row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := buf.String(); strings.Contains(html, "list-row-activity") {
		t.Errorf("an execution row must never render a card activity badge, got: %s", html)
	}
}

// The list-row-line3 container clamps to 2 lines and hides overflow (CSS),
// so a badge appended AFTER a long suggestion reason/summary can be clipped
// out of view. Activity badges must render first.
func TestTaskListRowMovement_CardActivity_RendersBeforeStatusContent(t *testing.T) {
	row := ListRow{
		Task:       &orchestrator.Task{ID: "t-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking},
		Suggestion: orchestrator.Suggestion{Verb: "go", Reason: "children specced"},
		Activity:   CardActivityState{WorkLabel: "Ready to run", CommandLabel: "Discuss: Launching"},
	}
	var buf bytes.Buffer
	if err := taskListRowMovement(row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	activityIdx := strings.Index(html, "list-row-activity")
	reasonIdx := strings.Index(html, "children specced")
	if activityIdx == -1 || reasonIdx == -1 {
		t.Fatalf("expected both the activity badge and the suggestion reason in the output, got: %s", html)
	}
	if activityIdx > reasonIdx {
		t.Errorf("activity badges must render before the status/suggestion content, got: %s", html)
	}
}

func TestTaskListRowMovement_CardNoActivity_RendersNoBadges(t *testing.T) {
	row := ListRow{
		Task: &orchestrator.Task{ID: "t-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked},
	}
	var buf bytes.Buffer
	if err := taskListRowMovement(row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := buf.String(); strings.Contains(html, "list-row-activity") {
		t.Errorf("a card with no active work child/command must render no activity badge, got: %s", html)
	}
}

// --- BuildListRows attaches Activity per task id ---

func TestBuildListRows_AttachesActivityByTaskID(t *testing.T) {
	tasks := []*orchestrator.Task{{ID: "t-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1"}}
	activity := map[string]CardActivityState{"t-1": {WorkLabel: "Draft"}}

	rows := BuildListRows(tasks, nil, nil, activity)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Activity.WorkLabel != "Draft" {
		t.Errorf("Activity = %+v, want WorkLabel=Draft", rows[0].Activity)
	}
}

func TestBuildListRows_NilActivityMap_ZeroValue(t *testing.T) {
	tasks := []*orchestrator.Task{{ID: "t-1", ProjectID: "proj-1"}}
	rows := BuildListRows(tasks, nil, nil, nil)
	if len(rows) != 1 || rows[0].Activity != (CardActivityState{}) {
		t.Errorf("Activity = %+v, want zero value", rows[0].Activity)
	}
}
