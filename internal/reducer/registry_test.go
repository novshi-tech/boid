package reducer_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/reducer"
)

func TestRegistry_Resolve_KnownBehavior(t *testing.T) {
	reg := reducer.NewDefaultRegistry()
	meta := &model.ProjectMeta{
		TaskBehaviors: map[string]model.TaskBehavior{
			"dev": {
				Name:       "development",
				Transition: "one-shot",
			},
		},
	}

	sm, err := reg.Resolve(meta, "dev")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sm.Name != "one-shot" {
		t.Fatalf("expected one-shot, got %s", sm.Name)
	}
}

func TestRegistry_Resolve_UnknownBehavior(t *testing.T) {
	reg := reducer.NewDefaultRegistry()
	meta := &model.ProjectMeta{
		TaskBehaviors: map[string]model.TaskBehavior{},
	}

	_, err := reg.Resolve(meta, "unknown")
	if err == nil {
		t.Fatal("expected error for unknown behavior")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestRegistry_Resolve_UnknownTransition(t *testing.T) {
	reg := reducer.NewDefaultRegistry()
	meta := &model.ProjectMeta{
		TaskBehaviors: map[string]model.TaskBehavior{
			"custom": {
				Name:       "custom",
				Transition: "nonexistent-machine",
			},
		},
	}

	_, err := reg.Resolve(meta, "custom")
	if err == nil {
		t.Fatal("expected error for unknown transition model")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestOneShotMachine_PendingToExecutingToDone(t *testing.T) {
	sm := reducer.OneShotMachine()

	task := &model.Task{Status: model.TaskStatusPending}

	// pending -> executing
	next, err := sm.Apply(task, &model.Action{Type: "start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if next.Status != model.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}

	// executing -> done
	next, err = sm.Apply(next, &model.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != model.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestOneShotMachine_InvalidTransition(t *testing.T) {
	sm := reducer.OneShotMachine()

	task := &model.Task{Status: model.TaskStatusPending}

	// pending -> done should fail (no direct transition)
	_, err := sm.Apply(task, &model.Action{Type: "done"})
	if err == nil {
		t.Fatal("expected error for invalid transition pending -> done")
	}
	if !strings.Contains(err.Error(), "no transition") {
		t.Fatalf("expected no transition error, got: %v", err)
	}
}

func TestOneShotMachine_AbortFromAny(t *testing.T) {
	sm := reducer.OneShotMachine()

	statuses := []model.TaskStatus{
		model.TaskStatusPending,
		model.TaskStatusExecuting,
	}

	for _, status := range statuses {
		task := &model.Task{Status: status}
		next, err := sm.Apply(task, &model.Action{Type: "abort"})
		if err != nil {
			t.Fatalf("abort from %s: %v", status, err)
		}
		if next.Status != model.TaskStatusAborted {
			t.Fatalf("expected aborted from %s, got %s", status, next.Status)
		}
	}
}

func TestOneShotMachine_JobFailedFromAny(t *testing.T) {
	sm := reducer.OneShotMachine()

	task := &model.Task{Status: model.TaskStatusExecuting}
	next, err := sm.Apply(task, &model.Action{Type: "job_failed"})
	if err != nil {
		t.Fatalf("job_failed: %v", err)
	}
	if next.Status != model.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", next.Status)
	}
}

func TestFeedbackLoopMachine_FullCycle(t *testing.T) {
	sm := reducer.FeedbackLoopMachine()

	task := &model.Task{Status: model.TaskStatusPending}

	// pending -> executing
	next, err := sm.Apply(task, &model.Action{Type: "start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if next.Status != model.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}

	// executing -> verifying
	next, err = sm.Apply(next, &model.Action{Type: "verify"})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if next.Status != model.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}

	// verifying -> in_review
	next, err = sm.Apply(next, &model.Action{Type: "review"})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if next.Status != model.TaskStatusInReview {
		t.Fatalf("expected in_review, got %s", next.Status)
	}

	// in_review -> collecting_feedback
	next, err = sm.Apply(next, &model.Action{Type: "collect_feedback"})
	if err != nil {
		t.Fatalf("collect_feedback: %v", err)
	}
	if next.Status != model.TaskStatusCollectingFeedback {
		t.Fatalf("expected collecting_feedback, got %s", next.Status)
	}

	// collecting_feedback -> executing (rework)
	next, err = sm.Apply(next, &model.Action{Type: "rework"})
	if err != nil {
		t.Fatalf("rework: %v", err)
	}
	if next.Status != model.TaskStatusExecuting {
		t.Fatalf("expected executing after rework, got %s", next.Status)
	}

	// Go through the cycle again and then finish
	next, err = sm.Apply(next, &model.Action{Type: "verify"})
	if err != nil {
		t.Fatalf("verify 2: %v", err)
	}
	next, err = sm.Apply(next, &model.Action{Type: "review"})
	if err != nil {
		t.Fatalf("review 2: %v", err)
	}
	next, err = sm.Apply(next, &model.Action{Type: "collect_feedback"})
	if err != nil {
		t.Fatalf("collect_feedback 2: %v", err)
	}

	// collecting_feedback -> done
	next, err = sm.Apply(next, &model.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != model.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestFeedbackLoopMachine_AbortFromAny(t *testing.T) {
	sm := reducer.FeedbackLoopMachine()

	statuses := []model.TaskStatus{
		model.TaskStatusPending,
		model.TaskStatusExecuting,
		model.TaskStatusVerifying,
		model.TaskStatusInReview,
		model.TaskStatusCollectingFeedback,
	}

	for _, status := range statuses {
		task := &model.Task{Status: status}
		next, err := sm.Apply(task, &model.Action{Type: "abort"})
		if err != nil {
			t.Fatalf("abort from %s: %v", status, err)
		}
		if next.Status != model.TaskStatusAborted {
			t.Fatalf("expected aborted from %s, got %s", status, next.Status)
		}
	}
}
