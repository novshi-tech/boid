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
		TaskStatusCaptured,
		TaskStatusTriaged,
		TaskStatusParked,
		TaskStatusReady,
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

func TestParseTaskStatus(t *testing.T) {
	cases := []struct {
		in     string
		want   TaskStatus
		wantOK bool
	}{
		{"pending", TaskStatusPending, true},
		{"captured", TaskStatusCaptured, true},
		{"triaged", TaskStatusTriaged, true},
		{"parked", TaskStatusParked, true},
		{"ready", TaskStatusReady, true},
		{"dropped", TaskStatusDropped, true},
		{"executing", TaskStatusExecuting, true},
		{"awaiting", TaskStatusAwaiting, true},
		{"done", TaskStatusDone, true},
		{"aborted", TaskStatusAborted, true},
		{"", "", false},
		{"garbage", "", false},
		{"PENDING", "", false}, // 大文字小文字を区別する（曖昧さを持ち込まない）
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
		TaskStatusCaptured, TaskStatusTriaged, TaskStatusParked, TaskStatusReady,
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

func TestIsPreExecutionStatus(t *testing.T) {
	pre := []TaskStatus{TaskStatusCaptured, TaskStatusTriaged, TaskStatusParked, TaskStatusReady}
	notPre := []TaskStatus{
		TaskStatusPending, TaskStatusExecuting, TaskStatusAwaiting,
		TaskStatusDone, TaskStatusAborted, TaskStatusDropped,
	}
	for _, s := range pre {
		if !IsPreExecutionStatus(s) {
			t.Errorf("IsPreExecutionStatus(%q) = false, want true", s)
		}
	}
	for _, s := range notPre {
		if IsPreExecutionStatus(s) {
			t.Errorf("IsPreExecutionStatus(%q) = true, want false", s)
		}
	}
}
