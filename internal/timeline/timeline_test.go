package timeline

import (
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestBuild_KeepsHookJobs(t *testing.T) {
	now := time.Now()
	task := &orchestrator.Task{Status: "executing", CreatedAt: now.Add(-10 * time.Second)}
	jobs := []*JobInfo{
		{ID: "j1", Role: "hook", HandlerID: "my-hook", Status: JobStatusCompleted, CreatedAt: now.Add(-5 * time.Second), UpdatedAt: now},
	}

	groups := Build(task, nil, jobs)

	found := false
	for _, g := range groups {
		for _, ev := range g.Events {
			if ev.Kind == KindJob && ev.Job.Role == "hook" {
				found = true
			}
		}
	}
	if !found {
		t.Error("hook job should be present in timeline, but was not found")
	}
}

func TestBuildJobLabel_DisplayNameOverridesHandlerID(t *testing.T) {
	now := time.Now()
	j := &JobInfo{
		Role:        "hook",
		HandlerID:   "my-kit/pr-verify",
		DisplayName: "PR Verify",
		Status:      JobStatusCompleted,
		CreatedAt:   now.Add(-5 * time.Second),
		UpdatedAt:   now,
	}
	label := BuildJobLabel(j)
	if !strings.Contains(label, "PR Verify") {
		t.Errorf("BuildJobLabel with DisplayName = %q: want label containing DisplayName, got %q", j.DisplayName, label)
	}
	if strings.Contains(label, "my-kit/pr-verify") {
		t.Errorf("BuildJobLabel with DisplayName set: should not contain HandlerID %q, got %q", j.HandlerID, label)
	}
}

func TestBuildJobLabel_FallsBackToHandlerID(t *testing.T) {
	now := time.Now()
	j := &JobInfo{
		Role:      "hook",
		HandlerID: "my-kit/pr-verify",
		Status:    JobStatusCompleted,
		CreatedAt: now.Add(-5 * time.Second),
		UpdatedAt: now,
	}
	label := BuildJobLabel(j)
	if !strings.Contains(label, "my-kit/pr-verify") {
		t.Errorf("BuildJobLabel without DisplayName should contain HandlerID, got %q", label)
	}
}

func TestBuildJobLabel_HookRoleOmitsPrefix(t *testing.T) {
	now := time.Now()
	j := &JobInfo{
		Role:      "hook",
		HandlerID: "my-kit/pr-verify",
		Status:    JobStatusCompleted,
		CreatedAt: now.Add(-5 * time.Second),
		UpdatedAt: now,
	}
	label := BuildJobLabel(j)
	if strings.Contains(label, "[hook]") {
		t.Errorf("BuildJobLabel with role=hook: should not contain '[hook]', got %q", label)
	}
	if !strings.Contains(label, "my-kit/pr-verify") {
		t.Errorf("BuildJobLabel with role=hook: should contain handler name, got %q", label)
	}
}

func TestBuildJobLabel_ExecutorRoleKeepsPrefix(t *testing.T) {
	now := time.Now()
	j := &JobInfo{
		Role:      "executor",
		HandlerID: "run-agent",
		Status:    JobStatusCompleted,
		CreatedAt: now.Add(-5 * time.Second),
		UpdatedAt: now,
	}
	label := BuildJobLabel(j)
	if !strings.Contains(label, "[executor]") {
		t.Errorf("BuildJobLabel with role=executor: should contain '[executor]', got %q", label)
	}
}

// A long-lived hook job that spans multiple ask/answer cycles (the
// `boid task ask` blocking RPC pattern) is created during the first
// executing visit and outlives later status changes. Without the Sticky
// reference row, only that first group surfaces the job and every subsequent
// awaiting/executing group looks empty even though the same agent process is
// still running there. This test pins the sticky-row behaviour to the
// latest status group for both task.Status=awaiting and task.Status=executing.
func TestBuild_StickyRunningJobAppearsInCurrentGroup(t *testing.T) {
	now := time.Now()
	taskCreated := now.Add(-10 * time.Minute)
	jobCreated := now.Add(-9 * time.Minute)

	jobs := []*JobInfo{
		{ID: "agent", Role: "hook", HandlerID: "claude-code", Status: JobStatusRunning, CreatedAt: jobCreated, UpdatedAt: now},
	}
	// First ask → awaiting, then answer → executing, then another ask →
	// awaiting. The single running job started during the first executing.
	actions := []*orchestrator.Action{
		{Type: "ask", FromStatus: orchestrator.TaskStatusExecuting, ToStatus: orchestrator.TaskStatusAwaiting, CreatedAt: now.Add(-8 * time.Minute)},
		{Type: "answer", FromStatus: orchestrator.TaskStatusAwaiting, ToStatus: orchestrator.TaskStatusExecuting, CreatedAt: now.Add(-6 * time.Minute)},
		{Type: "ask", FromStatus: orchestrator.TaskStatusExecuting, ToStatus: orchestrator.TaskStatusAwaiting, CreatedAt: now.Add(-2 * time.Minute)},
	}

	t.Run("task awaiting → sticky in latest awaiting group", func(t *testing.T) {
		task := &orchestrator.Task{Status: orchestrator.TaskStatusAwaiting, CreatedAt: taskCreated}
		groups := Build(task, actions, jobs)
		if len(groups) == 0 {
			t.Fatal("expected at least one status group")
		}
		last := groups[len(groups)-1]
		if last.Status != string(orchestrator.TaskStatusAwaiting) {
			t.Fatalf("last group should be awaiting, got %q", last.Status)
		}
		var sticky *Event
		for i := range last.Events {
			if last.Events[i].Sticky {
				sticky = &last.Events[i]
			}
		}
		if sticky == nil {
			t.Fatal("expected a Sticky=true event in the latest awaiting group")
		}
		if sticky.Job == nil || sticky.Job.ID != "agent" {
			t.Fatalf("sticky event should reference the running agent job, got %+v", sticky.Job)
		}
	})

	t.Run("task executing → sticky in latest executing group, not duplicated in first", func(t *testing.T) {
		// Simulate a state where the latest action is an answer → executing
		// (so the task is now back to executing again, with the job still
		// alive). Drop the trailing ask action.
		execActions := actions[:2]
		task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting, CreatedAt: taskCreated}
		groups := Build(task, execActions, jobs)
		if len(groups) < 3 {
			t.Fatalf("expected at least 3 groups (executing, awaiting, executing), got %d", len(groups))
		}
		// First executing already holds the job's authoritative row; sticky
		// must not be duplicated there.
		for _, ev := range groups[0].Events {
			if ev.Sticky {
				t.Fatal("first group already contains the job's authoritative row; sticky must not duplicate it there")
			}
		}
		// Last group is the new executing visit — sticky belongs here.
		last := groups[len(groups)-1]
		var sticky *Event
		for i := range last.Events {
			if last.Events[i].Sticky {
				sticky = &last.Events[i]
			}
		}
		if sticky == nil {
			t.Fatal("expected a Sticky=true event in the latest executing group")
		}
	})
}

// TestIsAnsweredAction / TestBuildActionLabel_Answered /
// TestBuild_IncludesAnsweredAction pin Opus review finding #6 (2026-08-19
// revisit of PR-3): `answered` is the FIRST non-transitioning action a
// human directly triggers by clicking a Web UI button (the Accept/Reject
// pair, J-6). Build's filter otherwise drops every non-transitioning,
// non-progress action (attrs_set/child_added/child_specced/noted included
// — that is a pre-existing gap this PR does not touch), which would leave
// a Reject click with literally no human-visible trace anywhere in the UI:
// the suggestion card just disappears. Decision: SHOW it — see
// IsAnsweredAction's own doc comment in timeline.go for the reasoning.

func TestIsStateTransition_SelfLoop(t *testing.T) {
	if !IsStateTransition(&orchestrator.Action{Type: "go", FromStatus: "working", ToStatus: "working"}) {
		t.Error("IsStateTransition(working -> working) = false, want true (card self-transition Go)")
	}
}

func TestBuild_IncludesSelfLoopAction(t *testing.T) {
	now := time.Now()
	task := &orchestrator.Task{Status: "working", CreatedAt: now.Add(-10 * time.Second)}
	actions := []*orchestrator.Action{
		{Type: "go", FromStatus: "working", ToStatus: "working", CreatedAt: now},
	}

	groups := Build(task, actions, nil)

	found := false
	for _, g := range groups {
		for _, ev := range g.Events {
			if ev.Kind == KindAction && ev.Action != nil && ev.Action.Type == "go" {
				found = true
				if ev.Label != "go → working" {
					t.Errorf("self-loop label = %q, want %q", ev.Label, "go → working")
				}
			}
		}
	}
	if !found {
		t.Error("self-loop go action should be present in timeline, but was not found")
	}
	if len(groups) != 1 {
		t.Errorf("self-loop should not open a new status group, got %d groups", len(groups))
	}
}

// TestIsStateTransition_NonGoSelfLoop_NotATransition pins that a
// non-"go" action carrying FromStatus == ToStatus (many non-transitioning
// action types get both fields stamped to the task's current, unchanged
// status for bookkeeping — see selfLoopTransitionTypes' own doc comment)
// is NOT treated as a state transition.
func TestIsStateTransition_NonGoSelfLoop_NotATransition(t *testing.T) {
	for _, typ := range []string{"progress", "attrs_set", "child_added", "child_specced", "noted", "hook_fired"} {
		if IsStateTransition(&orchestrator.Action{Type: typ, FromStatus: "executing", ToStatus: "executing"}) {
			t.Errorf("IsStateTransition(%s, executing -> executing) = true, want false", typ)
		}
	}
}

// TestBuild_ProgressAction_KeepsMessageLabel pins that a production-shaped
// progress action (FromStatus == ToStatus == the task's current status,
// per internal/api/task_notify.go) still renders its message text via
// buildProgressLabel, not as a bare "progress → executing" transition label.
func TestBuild_ProgressAction_KeepsMessageLabel(t *testing.T) {
	now := time.Now()
	task := &orchestrator.Task{Status: "executing", CreatedAt: now.Add(-10 * time.Second)}
	actions := []*orchestrator.Action{
		{Type: "progress", FromStatus: "executing", ToStatus: "executing", CreatedAt: now, Payload: []byte(`{"message":"halfway done"}`)},
	}

	groups := Build(task, actions, nil)

	found := false
	for _, g := range groups {
		for _, ev := range g.Events {
			if ev.Kind == KindAction && ev.Action != nil && ev.Action.Type == "progress" {
				found = true
				if !strings.Contains(ev.Label, "halfway done") {
					t.Errorf("progress label = %q, want it to contain the message", ev.Label)
				}
			}
		}
	}
	if !found {
		t.Error("progress action should be present in timeline, but was not found")
	}
}

// TestBuild_ExcludesNonTransitioningActionsWithStampedStatus pins that
// non-transitioning action types stamped with FromStatus == ToStatus (the
// generic ApplyAction path does this for every action, transitioning or
// not — see internal/api/workflow_action.go) stay excluded from the
// rendered timeline, same as when FromStatus/ToStatus are empty.
func TestBuild_ExcludesNonTransitioningActionsWithStampedStatus(t *testing.T) {
	now := time.Now()
	task := &orchestrator.Task{Status: "working", CreatedAt: now.Add(-10 * time.Second)}
	for _, typ := range []string{
		"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "hook_fired", "dispatch_error", "wake_due",
		orchestrator.ActionTypeCommandFinished, orchestrator.ActionTypeCommandFailed,
	} {
		actions := []*orchestrator.Action{
			{Type: typ, FromStatus: "working", ToStatus: "working", CreatedAt: now},
		}
		groups := Build(task, actions, nil)
		for _, g := range groups {
			for _, ev := range g.Events {
				if ev.Kind == KindAction && ev.Action != nil && ev.Action.Type == typ {
					t.Errorf("%s action should stay excluded from the timeline, but was rendered as %q", typ, ev.Label)
				}
			}
		}
	}
}

// TestSelfLoopTransitionTypes_MatchesMachineRules pins selfLoopTransitionTypes
// against both machines' actual rule tables: every Manual rule with
// FromStatus == ToStatus (both non-empty) must be either in the map or in
// this test's own deliberateOmissions set, so a future self-loop rule can't
// silently drift out of sync with either list.
func TestSelfLoopTransitionTypes_MatchesMachineRules(t *testing.T) {
	deliberateOmissions := map[string]bool{
		// execution machine's "abort: aborted -> aborted" self-loop: a
		// re-abort of an already-aborted task stays invisible in the
		// timeline, matching this package's pre-existing baseline.
		"abort": true,
	}

	found := map[string]bool{}
	for _, sm := range []*orchestrator.StateMachine{orchestrator.NewCardMachine(), orchestrator.NewExecutionMachine()} {
		for _, r := range sm.Rules {
			if r.Manual && r.FromStatus != "" && r.FromStatus != "*" && r.ToStatus != "" && r.FromStatus == r.ToStatus {
				found[r.Action] = true
			}
		}
	}

	for action := range found {
		if !selfLoopTransitionTypes[action] && !deliberateOmissions[action] {
			t.Errorf("machine rule table has a self-loop for action %q, but it is in neither selfLoopTransitionTypes nor this test's deliberateOmissions", action)
		}
	}
	for action := range selfLoopTransitionTypes {
		if !found[action] {
			t.Errorf("selfLoopTransitionTypes contains %q, but no machine rule table actually has a self-loop rule for it", action)
		}
	}
}

func TestIsAnsweredAction(t *testing.T) {
	if !IsAnsweredAction(&orchestrator.Action{Type: "answered"}) {
		t.Error("IsAnsweredAction(answered) = false, want true")
	}
	if IsAnsweredAction(&orchestrator.Action{Type: "noted"}) {
		t.Error("IsAnsweredAction(noted) = true, want false (noted stays out of scope for this PR)")
	}
	if IsAnsweredAction(&orchestrator.Action{Type: "attrs_set"}) {
		t.Error("IsAnsweredAction(attrs_set) = true, want false")
	}
}

func TestBuildActionLabel_Answered(t *testing.T) {
	cases := []struct {
		payload string
		want    string
	}{
		{`{"answer":"reject","verb":"go","basis":"issue #42"}`, "answered: reject"},
		{`{"answer":"accept"}`, "answered: accept"},
		{``, "answered"}, // malformed/empty payload falls back to the bare type
	}
	for _, c := range cases {
		a := &orchestrator.Action{Type: "answered", Payload: []byte(c.payload)}
		if got := BuildActionLabel(a); got != c.want {
			t.Errorf("BuildActionLabel(answered, payload=%s) = %q, want %q", c.payload, got, c.want)
		}
	}
}

func TestBuild_IncludesAnsweredAction(t *testing.T) {
	now := time.Now()
	// card-model-cleanup PR-2: orchestrator.TaskStatusTriaged no longer exists
	// as a Go constant (folded into "parked" by card machine v2 before this
	// PR), but Build reads Task.Status/Action.FromStatus/ToStatus as plain
	// untyped strings straight off the DB row — the actions table is never
	// rewritten by migration 0045, so a historical row can still legitimately
	// carry the literal "triaged" string. This test is about Build's
	// answered-action inclusion, not about status validity, so the raw
	// string literal (rather than the removed constant) keeps its original
	// intent — including verifying Build handles a legacy status gracefully.
	const legacyTriagedStatus = orchestrator.TaskStatus("triaged")
	task := &orchestrator.Task{Status: legacyTriagedStatus, CreatedAt: now.Add(-time.Hour)}
	actions := []*orchestrator.Action{
		{Type: "answered", FromStatus: legacyTriagedStatus, ToStatus: legacyTriagedStatus, CreatedAt: now.Add(-time.Minute), Payload: []byte(`{"answer":"reject"}`)},
	}
	groups := Build(task, actions, nil)

	found := false
	for _, g := range groups {
		for _, ev := range g.Events {
			if ev.Kind == KindAction && ev.Action != nil && ev.Action.Type == "answered" {
				found = true
				if !strings.Contains(ev.Label, "reject") {
					t.Errorf("answered event label = %q, want it to mention the reject decision", ev.Label)
				}
			}
		}
	}
	if !found {
		t.Fatal("answered action should appear in the timeline, but was not found")
	}
}

// Sticky reference rows are non-authoritative — they must not appear when
// the task has reached a terminal status (done / aborted) because the
// historic job placement is already correct, and any running job that
// outlived the task is itself an anomaly worth surfacing as-is rather than
// re-anchoring under a terminal group.
func TestBuild_StickyNotEmittedForTerminalTask(t *testing.T) {
	now := time.Now()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusAborted, CreatedAt: now.Add(-10 * time.Minute)}
	jobs := []*JobInfo{
		{ID: "agent", Role: "hook", HandlerID: "claude-code", Status: JobStatusRunning, CreatedAt: now.Add(-9 * time.Minute), UpdatedAt: now},
	}
	actions := []*orchestrator.Action{
		{Type: "abort", FromStatus: orchestrator.TaskStatusExecuting, ToStatus: orchestrator.TaskStatusAborted, CreatedAt: now.Add(-1 * time.Minute)},
	}
	groups := Build(task, actions, jobs)
	for _, g := range groups {
		for _, ev := range g.Events {
			if ev.Sticky {
				t.Fatalf("terminal task should not get a sticky row, but found one in group %q", g.Status)
			}
		}
	}
}
