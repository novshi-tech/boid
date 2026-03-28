package hook_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/hook"
	"github.com/novshi-tech/boid/internal/model"
)

func TestEvaluate_MatchingHookFires(t *testing.T) {
	eval := &hook.Evaluator{}

	task := &model.Task{
		Status:  model.TaskStatusExecuting,
		Payload: json.RawMessage(`{"agent_prompt":"do stuff"}`),
	}
	hooks := []model.Hook{
		{
			ID:             "run-agent",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitAgentPrompt},
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
	eval := &hook.Evaluator{}

	task := &model.Task{
		Status:  model.TaskStatusPending,
		Payload: json.RawMessage(`{"agent_prompt":"do stuff"}`),
	}
	hooks := []model.Hook{
		{
			ID:             "run-agent",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitAgentPrompt},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks for wrong status, got %d", len(matched))
	}
}

func TestEvaluate_MissingTrait(t *testing.T) {
	eval := &hook.Evaluator{}

	task := &model.Task{
		Status:  model.TaskStatusExecuting,
		Payload: json.RawMessage(`{"pr":"http://example.com"}`),
	}
	hooks := []model.Hook{
		{
			ID:             "run-agent",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitAgentPrompt},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks for missing trait, got %d", len(matched))
	}
}

func TestEvaluate_NoRequiredTraits(t *testing.T) {
	eval := &hook.Evaluator{}

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

func TestEvaluate_MultipleHooks(t *testing.T) {
	eval := &hook.Evaluator{}

	task := &model.Task{
		Status:  model.TaskStatusExecuting,
		Payload: json.RawMessage(`{"agent_prompt":"go","pr":"http://ex.com"}`),
	}
	hooks := []model.Hook{
		{
			ID:             "hook-a",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitAgentPrompt},
		},
		{
			ID:             "hook-b",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitPR},
		},
		{
			ID:             "hook-c",
			On:             "done",
			RequiresTraits: nil,
		},
		{
			ID:             "hook-d",
			On:             "executing",
			RequiresTraits: []model.TraitType{model.TraitPipeline},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 2 {
		t.Fatalf("expected 2 matched hooks, got %d", len(matched))
	}
	if matched[0].ID != "hook-a" {
		t.Fatalf("expected first match hook-a, got %s", matched[0].ID)
	}
	if matched[1].ID != "hook-b" {
		t.Fatalf("expected second match hook-b, got %s", matched[1].ID)
	}
}
