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
// A completed target with an attached request is finishing; a missing target
// supplies no evidence of the stage and must not be labeled running.
func TestCommandActivityLabel_AttachedTaskTarget_TerminalOrMissing(t *testing.T) {
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
		want := ""
		if tc.name == "done" || tc.name == "aborted" {
			want = "Discuss: Finishing"
		}
		if got := CommandActivityLabel(req, tc.statuses); got != want {
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
		if tc.status == orchestrator.TaskStatusDone {
			wantCommand = "Discuss: Finishing"
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

// Card list execution state is independent of the detail activity badges.
func TestTaskListRow_ExecutionBadge(t *testing.T) {
	for _, tt := range []struct {
		name        string
		state       orchestrator.CardExecutionState
		unavailable bool
		want        string
		absent      string
	}{
		{name: "idle", absent: "list-row-execution"},
		{name: "Go", state: orchestrator.CardExecutionState{Occupied: true}, want: "稼働中", absent: "__go__"},
		{name: "Discuss", state: orchestrator.CardExecutionState{Occupied: true, CommandLabel: "Discuss"}, want: "Discuss", absent: "入力待ち"},
		{name: "awaiting wins", state: orchestrator.CardExecutionState{Occupied: true, NeedsInput: true, CommandLabel: "Discuss"}, want: "入力待ち", absent: "稼働中"},
		{name: "awaiting without slot", state: orchestrator.CardExecutionState{NeedsInput: true}, want: "入力待ち", absent: "稼働中"},
		{name: "read failure", unavailable: true, want: "状態を取得できません", absent: "稼働中"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			row := ListRow{
				Task:     &orchestrator.Task{ID: "card", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking},
				Activity: tt.state, ActivityUnavailable: tt.unavailable,
				Summary: strings.Repeat("長いサマリー", 100),
			}
			var buf bytes.Buffer
			if err := taskListRow(row).Render(context.Background(), &buf); err != nil {
				t.Fatal(err)
			}
			html := buf.String()
			if tt.want != "" && !strings.Contains(html, tt.want) {
				t.Fatalf("missing %q: %s", tt.want, html)
			}
			if tt.absent != "" && strings.Contains(html, tt.absent) {
				t.Fatalf("unexpected %q: %s", tt.absent, html)
			}
			if tt.want != "" && strings.Index(html, "list-row-execution") > strings.Index(html, "list-row-line3") {
				t.Fatal("execution state must be outside the clamped summary line")
			}
		})
	}
}

func TestTaskListRow_ExecTaskNeverRendersCardExecutionBadge(t *testing.T) {
	row := ListRow{
		Task:     &orchestrator.Task{ID: "exec", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}},
		Activity: orchestrator.CardExecutionState{Occupied: true},
	}
	var buf bytes.Buffer
	if err := taskListRow(row).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "list-row-execution") {
		t.Fatal("execution task rendered a card badge")
	}
}

func TestBuildListRows_AttachesActivityByTaskID(t *testing.T) {
	tasks := []*orchestrator.Task{{ID: "t-1", Type: orchestrator.TaskTypeCard}, {ID: "t-2", Type: orchestrator.TaskTypeCard}}
	state := orchestrator.CardExecutionState{Occupied: true, NeedsInput: true}
	rows := BuildListRows(tasks, nil, nil, map[string]orchestrator.CardExecutionState{"t-1": state})
	if len(rows) != 2 || rows[0].Activity != state || rows[1].Activity != (orchestrator.CardExecutionState{}) {
		t.Fatalf("incorrect activity mapping: %+v", rows)
	}
}

func TestCommandActivityLabelTerminalTargetFinishing(t *testing.T) {
	req := &orchestrator.CardRequest{Status: orchestrator.CardRequestStatusAttached, TargetKind: orchestrator.CardRequestTargetKindTask, TargetID: "child", Launched: orchestrator.CardRequestDefinition{Label: "Inspect"}}
	if got := CommandActivityLabel(req, map[string]orchestrator.TaskStatus{"child": orchestrator.TaskStatusDone}); got != "Inspect: Finishing" {
		t.Fatalf("attached request with completed target = %q, want finishing", got)
	}
}
