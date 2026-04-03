package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestEvaluate_MatchingHookFires(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"do stuff"}}}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-agent",
			On:       "executing",
			Consumer: "claude-code",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitInstructions},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook, got %d", len(matched))
	}
	if matched[0].ID != "run-agent" {
		t.Fatalf("expected hook id run-agent, got %s", matched[0].ID)
	}
}

func TestEvaluate_NonMatchingStatus(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusPending,
		Payload: json.RawMessage(`{"instructions":"do stuff"}`),
	}
	hooks := []projectspec.Hook{
		{
			ID: "run-agent",
			On: "executing",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitInstructions},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks for wrong status, got %d", len(matched))
	}
}

func TestEvaluate_MissingTrait(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":"http://example.com"}`),
	}
	hooks := []projectspec.Hook{
		{
			ID: "run-agent",
			On: "executing",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitInstructions},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks for missing trait, got %d", len(matched))
	}
}

func TestEvaluate_NoRequiredTraits(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	hooks := []projectspec.Hook{
		{
			ID: "always-run",
			On: "executing",
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook (no traits required), got %d", len(matched))
	}
}

func TestInstructionTypeForStatus(t *testing.T) {
	cases := []struct {
		status   orchestrator.TaskStatus
		expected orchestrator.InstructionType
	}{
		{orchestrator.TaskStatusExecuting, orchestrator.InstructionTypeExecution},
		{orchestrator.TaskStatusCollectingFeedback, orchestrator.InstructionTypeExecution},
		{orchestrator.TaskStatusVerifying, orchestrator.InstructionTypeVerification},
		{orchestrator.TaskStatusInReview, orchestrator.InstructionTypeVerification},
		{orchestrator.TaskStatusPending, orchestrator.InstructionType("")},
	}
	for _, tc := range cases {
		got := orchestrator.InstructionTypeForStatus(tc.status)
		if got != tc.expected {
			t.Errorf("status %q: expected %q, got %q", tc.status, tc.expected, got)
		}
	}
}

func TestEvaluate_InstructionsRouting_ConsumerMatch(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"do something"}}}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-claude",
			On:       "executing",
			Consumer: "claude-code",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitInstructions},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook, got %d", len(matched))
	}
	if matched[0].ID != "run-claude" {
		t.Fatalf("expected hook id run-claude, got %s", matched[0].ID)
	}
}

func TestEvaluate_InstructionsRouting_ConsumerMismatch(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"do something"}}}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-codex",
			On:       "executing",
			Consumer: "codex",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitInstructions},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks (consumer mismatch), got %d", len(matched))
	}
}

func TestEvaluate_NonInstructionsHook_NotFiltered(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":"http://example.com"}`),
	}
	hooks := []projectspec.Hook{
		{
			ID: "handle-artifact",
			On: "executing",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook (consumer filter not applied), got %d", len(matched))
	}
	if matched[0].ID != "handle-artifact" {
		t.Fatalf("expected hook id handle-artifact, got %s", matched[0].ID)
	}
}

func TestEvaluate_InstructionsRouting_WrongStatus(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	// payload has execution-type instructions, but status is verifying
	// -> instType=verification, but no verification-type consumer in payload
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"do something"}}}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-claude",
			On:       "verifying",
			Consumer: "claude-code",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitInstructions},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks (wrong instruction type), got %d", len(matched))
	}
}

func TestEvaluateGates_MatchingGate(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":"http://example.com"}`),
	}
	gates := []projectspec.Gate{
		{
			ID: "push-pr",
			On: "executing",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	}

	matched := eval.EvaluateGates(task, gates)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched gate, got %d", len(matched))
	}
	if matched[0].ID != "push-pr" {
		t.Fatalf("expected gate id push-pr, got %s", matched[0].ID)
	}
}

func TestEvaluateGates_NonMatchingStatus(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusPending,
		Payload: json.RawMessage(`{"artifact":"url"}`),
	}
	gates := []projectspec.Gate{
		{
			ID: "push-pr",
			On: "executing",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	}

	matched := eval.EvaluateGates(task, gates)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched gates, got %d", len(matched))
	}
}
