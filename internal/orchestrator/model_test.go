package orchestrator

import "testing"

func TestKnownTaskStatuses(t *testing.T) {
	known := KnownTaskStatuses()
	want := []TaskStatus{
		TaskStatusPending,
		TaskStatusExecuting,
		TaskStatusAwaiting,
		TaskStatusDone,
		TaskStatusAborted,
		TaskStatusParked,
		TaskStatusWorking,
		TaskStatusDropped,
	}
	if len(known) != len(want) {
		t.Fatalf("KnownTaskStatuses() = %v, want %v", known, want)
	}
	seen := make(map[TaskStatus]bool, len(known))
	for _, s := range known {
		seen[s] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("KnownTaskStatuses() missing %q", w)
		}
	}
}

// TestKnownTaskStatuses_LegacyStatusesGone pins card-model-cleanup PR-2
// (migration 0045, design doc §3.3): captured/triaged/ready are no longer
// valid tasks.status values at all — the DB's CHECK constraint rejects them
// — so KnownTaskStatuses must not accept them either (a validator that still
// recognized them would let a caller create a row the DB then refuses).
func TestKnownTaskStatuses_LegacyStatusesGone(t *testing.T) {
	for _, legacy := range []string{"captured", "triaged", "ready"} {
		if _, ok := ParseTaskStatus(legacy); ok {
			t.Errorf("ParseTaskStatus(%q) = ok, want unrecognized (legacy status removed by card-model-cleanup PR-2)", legacy)
		}
	}
}

func TestParseTaskStatus(t *testing.T) {
	cases := []struct {
		in     string
		want   TaskStatus
		wantOK bool
	}{
		{"pending", TaskStatusPending, true},
		{"parked", TaskStatusParked, true},
		{"dropped", TaskStatusDropped, true},
		{"executing", TaskStatusExecuting, true},
		{"awaiting", TaskStatusAwaiting, true},
		{"done", TaskStatusDone, true},
		{"aborted", TaskStatusAborted, true},
		{"working", TaskStatusWorking, true},
		{"", "", false},
		{"garbage", "", false},
		{"captured", "", false}, // legacy, removed by card-model-cleanup PR-2
		{"triaged", "", false},  // legacy, removed by card-model-cleanup PR-2
		{"ready", "", false},    // legacy, removed by card-model-cleanup PR-2
		{"PENDING", "", false},  // 大文字小文字を区別する（曖昧さを持ち込まない）
	}
	for _, c := range cases {
		got, ok := ParseTaskStatus(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("ParseTaskStatus(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestIsTerminalStatus(t *testing.T) {
	terminal := []TaskStatus{TaskStatusDone, TaskStatusAborted, TaskStatusDropped}
	nonTerminal := []TaskStatus{
		TaskStatusPending, TaskStatusExecuting, TaskStatusAwaiting,
		TaskStatusParked, TaskStatusWorking,
	}
	for _, s := range terminal {
		if !IsTerminalStatus(s) {
			t.Errorf("IsTerminalStatus(%q) = false, want true", s)
		}
	}
	for _, s := range nonTerminal {
		if IsTerminalStatus(s) {
			t.Errorf("IsTerminalStatus(%q) = true, want false", s)
		}
	}
}

// TestIsPreDispatchEditableStatus pins card-model-cleanup PR-2's type-aware
// replacement for the old status-only predicate (model.go's own doc
// comment): a card is editable only in "parked", an execution task only in
// "pending" — no other (type, status) combination is editable, including
// the shared "done" status on either side.
func TestIsPreDispatchEditableStatus(t *testing.T) {
	cases := []struct {
		taskType TaskType
		status   TaskStatus
		want     bool
	}{
		{TaskTypeCard, TaskStatusParked, true},
		{TaskTypeCard, TaskStatusWorking, false},
		{TaskTypeCard, TaskStatusDone, false},
		{TaskTypeCard, TaskStatusDropped, false},
		{TaskTypeExecution, TaskStatusPending, true},
		{TaskTypeExecution, TaskStatusExecuting, false},
		{TaskTypeExecution, TaskStatusAwaiting, false},
		{TaskTypeExecution, TaskStatusDone, false},
		{TaskTypeExecution, TaskStatusAborted, false},
	}
	for _, c := range cases {
		got := IsPreDispatchEditableStatus(c.taskType, c.status)
		if got != c.want {
			t.Errorf("IsPreDispatchEditableStatus(%q, %q) = %v, want %v", c.taskType, c.status, got, c.want)
		}
	}
}

// TestIsInstructionsEditable pins the same type-aware narrowing for
// Instructions specifically (lifecycle.go): execution-only, pending-only —
// see IsInstructionsEditable's own doc comment for why this is narrower than
// IsPreDispatchEditableStatus's card allowance (Instructions is an
// execution-only field; a card has none to edit in any status).
func TestIsInstructionsEditable(t *testing.T) {
	cases := []struct {
		taskType TaskType
		status   TaskStatus
		want     bool
	}{
		{TaskTypeExecution, TaskStatusPending, true},
		{TaskTypeExecution, TaskStatusExecuting, false},
		{TaskTypeExecution, TaskStatusDone, false},
		{TaskTypeCard, TaskStatusParked, false},
		{TaskTypeCard, TaskStatusWorking, false},
	}
	for _, c := range cases {
		got := IsInstructionsEditable(c.taskType, c.status)
		if got != c.want {
			t.Errorf("IsInstructionsEditable(%q, %q) = %v, want %v", c.taskType, c.status, got, c.want)
		}
	}
}

// TestValidateTaskTypeConsistency pins Q17 (docs/plans/card-model-cleanup.md
// §10): the Type/non-nil-side invariant is checked, not merely documented.
func TestValidateTaskTypeConsistency(t *testing.T) {
	cases := []struct {
		name    string
		task    Task
		wantErr bool
	}{
		{"card with Card set", Task{Type: TaskTypeCard, Card: &CardAttrs{}}, false},
		{"card with Exec set", Task{Type: TaskTypeCard, Card: &CardAttrs{}, Exec: &ExecAttrs{}}, true},
		{"card with nil Card", Task{Type: TaskTypeCard}, true},
		{"execution with Exec set", Task{Type: TaskTypeExecution, Exec: &ExecAttrs{}}, false},
		{"execution with Card set", Task{Type: TaskTypeExecution, Exec: &ExecAttrs{}, Card: &CardAttrs{}}, true},
		{"execution with nil Exec", Task{Type: TaskTypeExecution}, true},
		{"unknown type", Task{Type: "bogus"}, true},
	}
	for _, c := range cases {
		err := validateTaskTypeConsistency(&c.task)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateTaskTypeConsistency() error = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

// TestCloneTaskShallow_ExecAndCardAreIndependentCopies pins the exact bug
// class CloneTaskShallow exists to prevent (see its own doc comment): a bare
// `clone := *task` copies the Exec/Card POINTER, not what it points to, so
// mutating a field on the clone's Exec/Card would alias back into the
// original task's Exec/Card without this helper. Before the tagged-struct
// split this class of bug was impossible (Payload/Instructions/etc were
// value-typed fields on Task itself, copied by the struct copy); every call
// site that used to do a bare `newTask := *task` and then mutate a
// now-relocated field (machine.go's Apply/AdvanceFull, coordinator.go's
// evaluateAndAdvance, api.attrs_set_done.go's done-status noop) had to
// switch to CloneTaskShallow for this exact reason — this test is the
// direct regression pin for that class of fix.
func TestCloneTaskShallow_ExecAndCardAreIndependentCopies(t *testing.T) {
	orig := &Task{
		Type:   TaskTypeExecution,
		Status: TaskStatusExecuting,
		Exec:   &ExecAttrs{Behavior: "orig-behavior", Payload: []byte(`{"a":1}`)},
	}
	clone := CloneTaskShallow(orig)
	if clone.Exec == orig.Exec {
		t.Fatal("CloneTaskShallow: clone.Exec is the SAME pointer as orig.Exec (aliased, not copied)")
	}
	clone.Exec.Payload = []byte(`{"a":2}`)
	clone.Exec.Behavior = "clone-behavior"
	if string(orig.Exec.Payload) != `{"a":1}` {
		t.Errorf("mutating clone.Exec.Payload changed orig.Exec.Payload too: got %s, want unchanged {\"a\":1}", orig.Exec.Payload)
	}
	if orig.Exec.Behavior != "orig-behavior" {
		t.Errorf("mutating clone.Exec.Behavior changed orig.Exec.Behavior too: got %q, want unchanged %q", orig.Exec.Behavior, "orig-behavior")
	}

	origCard := &Task{
		Type:   TaskTypeCard,
		Status: TaskStatusParked,
		Card:   &CardAttrs{Kind: "issue", Detail: []byte(`{}`)},
	}
	cloneCard := CloneTaskShallow(origCard)
	if cloneCard.Card == origCard.Card {
		t.Fatal("CloneTaskShallow: cloneCard.Card is the SAME pointer as origCard.Card (aliased, not copied)")
	}
	cloneCard.Card.Kind = "signal"
	if origCard.Card.Kind != "issue" {
		t.Errorf("mutating cloneCard.Card.Kind changed origCard.Card.Kind too: got %q, want unchanged %q", origCard.Card.Kind, "issue")
	}

	if CloneTaskShallow(nil) != nil {
		t.Error("CloneTaskShallow(nil) should return nil")
	}
}
