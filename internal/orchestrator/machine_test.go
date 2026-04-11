package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestDefaultMachine_ManualActions verifies all manual action transitions.
func TestDefaultMachine_ManualActions(t *testing.T) {
	sm := orchestrator.DefaultMachine()

	cases := []struct {
		name       string
		fromStatus orchestrator.TaskStatus
		action     string
		wantStatus orchestrator.TaskStatus
		wantErr    bool
	}{
		{"start: pending→executing", orchestrator.TaskStatusPending, "start", orchestrator.TaskStatusExecuting, false},
		{"done: executing→done", orchestrator.TaskStatusExecuting, "done", orchestrator.TaskStatusDone, false},
		{"done: verifying→done", orchestrator.TaskStatusVerifying, "done", orchestrator.TaskStatusDone, false},
		{"done: reworking→done", orchestrator.TaskStatusReworking, "done", orchestrator.TaskStatusDone, false},
		{"reopen: done→reworking", orchestrator.TaskStatusDone, "reopen", orchestrator.TaskStatusReworking, false},
		{"abort: pending→aborted", orchestrator.TaskStatusPending, "abort", orchestrator.TaskStatusAborted, false},
		{"abort: executing→aborted", orchestrator.TaskStatusExecuting, "abort", orchestrator.TaskStatusAborted, false},
		{"abort: verifying→aborted", orchestrator.TaskStatusVerifying, "abort", orchestrator.TaskStatusAborted, false},
		{"abort: reworking→aborted", orchestrator.TaskStatusReworking, "abort", orchestrator.TaskStatusAborted, false},
		{"job_failed: pending→aborted", orchestrator.TaskStatusPending, "job_failed", orchestrator.TaskStatusAborted, false},
		{"job_failed: executing→aborted", orchestrator.TaskStatusExecuting, "job_failed", orchestrator.TaskStatusAborted, false},
		{"job_failed: reworking→aborted", orchestrator.TaskStatusReworking, "job_failed", orchestrator.TaskStatusAborted, false},
		// invalid transitions
		{"start: executing (invalid)", orchestrator.TaskStatusExecuting, "start", "", true},
		{"done: pending (invalid)", orchestrator.TaskStatusPending, "done", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &orchestrator.Task{Status: tc.fromStatus}
			next, err := sm.Apply(task, &orchestrator.Action{Type: tc.action})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Apply(%q from %q): expected error, got nil", tc.action, tc.fromStatus)
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply(%q from %q): %v", tc.action, tc.fromStatus, err)
			}
			if next.Status != tc.wantStatus {
				t.Errorf("Apply(%q from %q) = %q, want %q", tc.action, tc.fromStatus, next.Status, tc.wantStatus)
			}
		})
	}
}

// TestDefaultMachine_Advance_Executing_TasksReady verifies plan task auto-advance to done.
func TestDefaultMachine_Advance_Executing_TasksReady(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"tasks":[{"title":"subtask"}]}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when tasks ready")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Errorf("Advance = %q, want done", next.Status)
	}
}

// TestDefaultMachine_Advance_Executing_ArtifactNoFindings verifies simple impl auto-advance to verifying.
func TestDefaultMachine_Advance_Executing_ArtifactNoFindings(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when artifact present and no findings")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Errorf("Advance = %q, want verifying", next.Status)
	}
}

// TestDefaultMachine_Advance_Executing_ArtifactWithOpenFindings verifies CI-loop advance to reworking.
func TestDefaultMachine_Advance_Executing_ArtifactWithOpenFindings(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"github-pr-verification/pr-verify":{
					"source_state":"executing",
					"findings":[{"message":"GitHub Actions failed","status":"open"}]
				}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to reworking when artifact present and open findings")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Errorf("Advance = %q, want reworking", next.Status)
	}
}

// TestDefaultMachine_Advance_Executing_ArtifactWithResolvedFindings verifies CI-loop done advance.
func TestDefaultMachine_Advance_Executing_ArtifactWithResolvedFindings(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"github-pr-verification/pr-verify":{
					"source_state":"executing",
					"findings":[{"message":"GitHub Actions passed","status":"resolved"}]
				}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when artifact and all executing findings resolved")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Errorf("Advance = %q, want verifying", next.Status)
	}
}

// TestDefaultMachine_Advance_Executing_NoArtifact_NoAdvance verifies no advance without artifact.
func TestDefaultMachine_Advance_Executing_NoArtifact_NoAdvance(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	_, ok := sm.Advance(task)
	if ok {
		t.Fatal("expected no advance when no artifact and no tasks")
	}
}

// TestDefaultMachine_Advance_Verifying_OpenFindings_ToReworking verifies verify-gate loop.
func TestDefaultMachine_Advance_Verifying_OpenFindings_ToReworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"verification":{
				"verify-gate":{
					"source_state":"verifying",
					"findings":[{"message":"review failed","status":"open"}]
				}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to reworking when verifying findings open")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Errorf("Advance = %q, want reworking", next.Status)
	}
}

// TestDefaultMachine_Advance_Verifying_NoFindings_Done verifies pass-through to done.
func TestDefaultMachine_Advance_Verifying_NoFindings_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when no verification findings")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Errorf("Advance = %q, want done", next.Status)
	}
}

// TestDefaultMachine_Advance_Verifying_AllResolved_Done verifies advance to done when all resolved.
func TestDefaultMachine_Advance_Verifying_AllResolved_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"verification":{
				"verify-gate":{
					"source_state":"verifying",
					"findings":[{"message":"ok","status":"resolved"}]
				}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when verifying findings resolved")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Errorf("Advance = %q, want done", next.Status)
	}
}

// TestDefaultMachine_Advance_Reworking_UnresolvedFindings_SelfLoop verifies CI fix loop.
func TestDefaultMachine_Advance_Reworking_UnresolvedFindings_SelfLoop(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"verification":{
				"pr-verify":{
					"source_state":"executing",
					"findings":[{"message":"CI failed","status":"open"}]
				}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected self-loop in reworking when unresolved findings")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Errorf("Advance = %q, want reworking (self-loop)", next.Status)
	}
}

// TestDefaultMachine_Advance_Reworking_AllResolved_Done verifies resolution after rework.
func TestDefaultMachine_Advance_Reworking_AllResolved_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"verification":{
				"pr-verify":{
					"source_state":"reworking",
					"findings":[{"message":"CI passed","status":"resolved"}]
				}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when all findings resolved")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Errorf("Advance = %q, want done", next.Status)
	}
}

// TestDefaultMachine_Advance_Reworking_NoFindings_Done verifies immediate done when no findings.
func TestDefaultMachine_Advance_Reworking_NoFindings_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when no findings")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Errorf("Advance = %q, want done", next.Status)
	}
}

// TestDefaultMachine_AvailableActions verifies manual action listings per status.
func TestDefaultMachine_AvailableActions(t *testing.T) {
	sm := orchestrator.DefaultMachine()

	cases := []struct {
		status  orchestrator.TaskStatus
		want    map[string]bool
		wantLen int
	}{
		{
			status:  orchestrator.TaskStatusPending,
			want:    map[string]bool{"start": true, "abort": true},
			wantLen: 2,
		},
		{
			status:  orchestrator.TaskStatusExecuting,
			want:    map[string]bool{"done": true, "abort": true},
			wantLen: 2,
		},
		{
			status:  orchestrator.TaskStatusVerifying,
			want:    map[string]bool{"done": true, "abort": true},
			wantLen: 2,
		},
		{
			status:  orchestrator.TaskStatusReworking,
			want:    map[string]bool{"done": true, "abort": true},
			wantLen: 2,
		},
		{
			status:  orchestrator.TaskStatusDone,
			want:    map[string]bool{},
			wantLen: 0,
		},
		{
			status:  orchestrator.TaskStatusAborted,
			want:    map[string]bool{},
			wantLen: 0,
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			actions := sm.AvailableActions(tc.status)
			if len(actions) != tc.wantLen {
				t.Errorf("AvailableActions(%q) = %v (len %d), want len %d", tc.status, actions, len(actions), tc.wantLen)
			}
			for _, a := range actions {
				if !tc.want[a] {
					t.Errorf("unexpected action %q in AvailableActions(%q)", a, tc.status)
				}
				if a == "job_failed" {
					t.Errorf("job_failed must not appear in AvailableActions(%q)", tc.status)
				}
			}
		})
	}
}

// TestDefaultMachine_Apply_IgnoresConditionRules verifies that condition-based rules
// are skipped by Apply.
func TestDefaultMachine_Apply_IgnoresConditionRules(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "job_completed"})
	if err == nil {
		t.Error("Apply(job_completed) should return error for condition-only transitions")
	}
}

// TestStateMachine_Advance_ConditionMet verifies the generic Advance behavior.
func TestStateMachine_Advance_ConditionMet(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				FromStatus: "executing",
				ToStatus:   "verifying",
				Condition: func(payload json.RawMessage) bool {
					var m map[string]json.RawMessage
					json.Unmarshal(payload, &m)
					_, ok := m["artifact"]
					return ok
				},
			},
		},
	}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":{"url":"https://github.com/..."}}`),
	}

	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected Advance to return ok=true")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

// TestStateMachine_Apply_IgnoresConditionRules verifies Apply skips condition-based rules.
func TestStateMachine_Apply_IgnoresConditionRules(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{
				FromStatus: "executing",
				ToStatus:   "verifying",
				Condition: func(payload json.RawMessage) bool {
					return true
				},
			},
		},
	}

	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "verify"})
	if err == nil {
		t.Fatal("Apply should not match condition-based rules via action")
	}
}
