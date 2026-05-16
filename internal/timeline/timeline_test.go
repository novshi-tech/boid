package timeline

import (
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestBuild_DropsGateJobs(t *testing.T) {
	now := time.Now()
	task := &orchestrator.Task{Status: "executing", CreatedAt: now.Add(-10 * time.Second)}
	actions := []*orchestrator.Action{
		{Type: "start", FromStatus: "pending", ToStatus: "executing", CreatedAt: now.Add(-5 * time.Second)},
	}
	jobs := []*JobInfo{
		{ID: "j1", Role: "gate", Status: JobStatusCompleted, CreatedAt: now.Add(-4 * time.Second), UpdatedAt: now.Add(-3 * time.Second)},
		{ID: "j2", Role: "exit_gate", Status: JobStatusCompleted, CreatedAt: now.Add(-3 * time.Second), UpdatedAt: now.Add(-2 * time.Second)},
		{ID: "j3", Role: "entry_gate", Status: JobStatusCompleted, CreatedAt: now.Add(-2 * time.Second), UpdatedAt: now.Add(-time.Second)},
		{ID: "j4", Role: "hook", Status: JobStatusCompleted, CreatedAt: now.Add(-time.Second), UpdatedAt: now},
	}

	groups := Build(task, actions, jobs)

	for _, g := range groups {
		for _, ev := range g.Events {
			if ev.Kind != KindJob {
				continue
			}
			if IsGateRole(ev.Job.Role) {
				t.Errorf("gate job role %q should be excluded from timeline", ev.Job.Role)
			}
		}
	}
}

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

func TestIsGateRole(t *testing.T) {
	gateRoles := []string{"gate", "exit_gate", "entry_gate"}
	for _, r := range gateRoles {
		if !IsGateRole(r) {
			t.Errorf("IsGateRole(%q) = false, want true", r)
		}
	}
	nonGateRoles := []string{"hook", "exec", "", "dispatcher"}
	for _, r := range nonGateRoles {
		if IsGateRole(r) {
			t.Errorf("IsGateRole(%q) = true, want false", r)
		}
	}
}
