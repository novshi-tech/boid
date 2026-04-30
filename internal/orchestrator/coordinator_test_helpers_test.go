package orchestrator_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// mockExecutorWaiter implements HookExecutor, GateExecutor, and JobWaiter.
type mockExecutorWaiter struct {
	mu          sync.Mutex
	hookCalls   []*projectspec.HookFireEvent
	gateCalls   []*projectspec.GateFireEvent
	jobCounter  int
	completions map[string]orchestrator.JobCompletion
	execOrder   []string
}

func newMockExecutorWaiter() *mockExecutorWaiter {
	return &mockExecutorWaiter{
		completions: make(map[string]orchestrator.JobCompletion),
	}
}

func (m *mockExecutorWaiter) setHookCompletion(hookID string, output string, exitCode int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobCounter++
	jobID := fmt.Sprintf("job-%s-%d", hookID, m.jobCounter)
	m.completions[jobID] = orchestrator.JobCompletion{
		JobID:    jobID,
		Output:   output,
		ExitCode: exitCode,
	}
	return jobID
}

func (m *mockExecutorWaiter) setGateCompletion(gateID string, output string, exitCode int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobCounter++
	jobID := fmt.Sprintf("job-%s-%d", gateID, m.jobCounter)
	m.completions[jobID] = orchestrator.JobCompletion{
		JobID:    jobID,
		Output:   output,
		ExitCode: exitCode,
	}
	return jobID
}

func (m *mockExecutorWaiter) findJobForID(id string) string {
	prefix := "job-" + id + "-"
	for jobID := range m.completions {
		if len(jobID) >= len(prefix) && jobID[:len(prefix)] == prefix {
			return jobID
		}
	}
	return ""
}

func (m *mockExecutorWaiter) ExecuteHook(ctx context.Context, event *projectspec.HookFireEvent) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hookCalls = append(m.hookCalls, event)
	m.execOrder = append(m.execOrder, "hook:"+event.Hook.ID)
	if jobID := m.findJobForID(event.Hook.ID); jobID != "" {
		return jobID, nil
	}
	m.jobCounter++
	jobID := fmt.Sprintf("job-%s-%d", event.Hook.ID, m.jobCounter)
	m.completions[jobID] = orchestrator.JobCompletion{JobID: jobID, Output: `{"payload_patch":{}}`, ExitCode: 0}
	return jobID, nil
}

func (m *mockExecutorWaiter) ExecuteGate(ctx context.Context, event *projectspec.GateFireEvent) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gateCalls = append(m.gateCalls, event)
	m.execOrder = append(m.execOrder, "gate:"+event.Gate.ID)
	if jobID := m.findJobForID(event.Gate.ID); jobID != "" {
		return jobID, nil
	}
	m.jobCounter++
	jobID := fmt.Sprintf("job-%s-%d", event.Gate.ID, m.jobCounter)
	m.completions[jobID] = orchestrator.JobCompletion{JobID: jobID, Output: `{"payload_patch":{}}`, ExitCode: 0}
	return jobID, nil
}

// WaitForJob mirrors the production WaitForJobCtx contract: a non-zero exit
// is NOT reported as an error — callers are expected to inspect the returned
// JobCompletion.ExitCode. Only true wait-machinery failures (e.g. unknown job)
// produce a non-nil error here.
func (m *mockExecutorWaiter) WaitForJob(ctx context.Context, jobID string) (orchestrator.JobCompletion, error) {
	m.mu.Lock()
	c, ok := m.completions[jobID]
	m.mu.Unlock()
	if !ok {
		return orchestrator.JobCompletion{}, fmt.Errorf("unknown job: %s", jobID)
	}
	return c, nil
}

// metaWithBehavior builds a ProjectMeta that exposes hooks/gates via a single
// "dev" behavior. Tests pair this with tasks whose Behavior is "dev".
func metaWithBehavior(hooks []projectspec.Hook, gates []projectspec.Gate) *projectspec.ProjectMeta {
	return &projectspec.ProjectMeta{
		TaskBehaviors: map[string]projectspec.TaskBehavior{
			"dev": {Name: "dev", Hooks: hooks, Gates: gates},
		},
	}
}

func simpleStateMachine() *orchestrator.StateMachine {
	return &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				FromStatus: "executing",
				ToStatus:   "done",
				Condition: func(p json.RawMessage) bool {
					var m map[string]json.RawMessage
					json.Unmarshal(p, &m)
					_, ok := m["prompt"]
					return ok && string(m["prompt"]) != "null"
				},
			},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted"},
		},
	}
}

// lifecycleExecutedStateMachine は lifecycle.executed=true を done 遷移条件に
// 持つ state machine を返す (plan behavior の典型形)。 hook 失敗時に誤って
// lifecycle.executed=true が立つバグの再現に使う。
func lifecycleExecutedStateMachine() *orchestrator.StateMachine {
	return &orchestrator.StateMachine{
		Name: "lifecycle-executed",
		Rules: []orchestrator.Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				FromStatus: "executing",
				ToStatus:   "done",
				Condition: func(p json.RawMessage) bool {
					var m map[string]json.RawMessage
					if err := json.Unmarshal(p, &m); err != nil {
						return false
					}
					lc, ok := m["lifecycle"]
					if !ok {
						return false
					}
					var lcMap map[string]json.RawMessage
					if err := json.Unmarshal(lc, &lcMap); err != nil {
						return false
					}
					return string(lcMap["executed"]) == "true"
				},
			},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted"},
		},
	}
}
