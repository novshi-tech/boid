package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestEvaluate_MatchingHookFires(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &model.Task{
		Status:  model.TaskStatusExecuting,
		Payload: json.RawMessage(`{"prompt":"do stuff"}`),
	}
	hooks := []model.Hook{
		{
			ID:             "run-agent",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitPrompt},
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

	task := &model.Task{
		Status:  model.TaskStatusPending,
		Payload: json.RawMessage(`{"prompt":"do stuff"}`),
	}
	hooks := []model.Hook{
		{
			ID:             "run-agent",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitPrompt},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks for wrong status, got %d", len(matched))
	}
}

func TestEvaluate_MissingTrait(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &model.Task{
		Status:  model.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":"http://example.com"}`),
	}
	hooks := []model.Hook{
		{
			ID:             "run-agent",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitPrompt},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks for missing trait, got %d", len(matched))
	}
}

func TestEvaluate_NoRequiredTraits(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &model.Task{
		Status:  model.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	hooks := []model.Hook{
		{
			ID:             "always-run",
			On:             "executing",
			RequiresTraits: nil,
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook (no traits required), got %d", len(matched))
	}
}

func TestEvaluateGates_MatchingGate(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &model.Task{
		Status:  model.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":"http://example.com"}`),
	}
	gates := []model.Gate{
		{
			ID:             "push-pr",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitArtifact},
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

	task := &model.Task{
		Status:  model.TaskStatusPending,
		Payload: json.RawMessage(`{"artifact":"url"}`),
	}
	gates := []model.Gate{
		{
			ID:             "push-pr",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitArtifact},
		},
	}

	matched := eval.EvaluateGates(task, gates)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched gates, got %d", len(matched))
	}
}
