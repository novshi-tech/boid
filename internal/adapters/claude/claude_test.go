package claude

import (
	"context"
	"testing"
)

// Phase 3-b drastically shrank the adapter surface: Run() owns the entire
// agent process lifecycle (covered by run_test.go and the end-to-end
// runner-inner-child path), and Usage() is the only remaining query
// method. The dropped StopAgent / ResumePayload / Interactive /
// SessionIDFromHookEnv methods were exercised by tests that have been
// removed alongside their implementations — graceful stop is now delivered
// by api.JobLifecycle.SignalJobRuntime, resume / session-id handoff flows
// through adapters.RunContext.SessionID, and PTY allocation is decided by
// dispatcher / planner from spec.HarnessType.
func TestAdapter_Usage_ReturnsZeroStub(t *testing.T) {
	a := New()
	u, err := a.Usage(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u.Model != "" || u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("Usage stub should return zero Usage, got %+v", u)
	}
}
