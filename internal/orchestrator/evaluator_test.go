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
		Status: orchestrator.TaskStatusExecuting,
		Instructions: orchestrator.Instructions{
			{Agent: "claude-code", Message:"do stuff"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-agent",
			Kind:     projectspec.HandlerKindAgent,
			Agent: "claude-code",
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
		Payload: json.RawMessage(`{}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:   "run-agent",
			Kind: projectspec.HandlerKindAgent,
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks for wrong status, got %d", len(matched))
	}
}

func TestEvaluate_MissingTrait(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	// artifact trait を require する hook は、payload に artifact が無いと fire しない
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	hooks := []projectspec.Hook{
		{
			ID: "run-agent",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
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
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook (no traits required), got %d", len(matched))
	}
}

func TestEvaluate_InstructionsRouting_AgentMatch(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Instructions: orchestrator.Instructions{
			{Agent: "claude-code", Message:"do something"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-claude",
			Kind:     projectspec.HandlerKindAgent,
			Agent: "claude-code",
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

func TestEvaluate_InstructionsRouting_AgentMismatch(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Instructions: orchestrator.Instructions{
			{Agent: "claude-code", Message:"do something"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-opencode",
			Kind:     projectspec.HandlerKindAgent,
			Agent: "opencode",
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched hooks (agent mismatch), got %d", len(matched))
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
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook (agent filter not applied), got %d", len(matched))
	}
	if matched[0].ID != "handle-artifact" {
		t.Fatalf("expected hook id handle-artifact, got %s", matched[0].ID)
	}
}

func TestEvaluate_OptionalTrait_FiresWhenAbsent(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	// executing: verification not in payload yet — hook should still fire
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
		Instructions: orchestrator.Instructions{
			{Agent: "claude-code", Message:"impl"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-agent",
			Agent: "claude-code",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{"verification?"},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook (optional trait absent), got %d", len(matched))
	}
}

func TestEvaluate_KitIDPreservedInMatchedHook(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:  "go-dev/pr-verify",
			Kit: "go-dev",
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook, got %d", len(matched))
	}
	if matched[0].Kit != "go-dev" {
		t.Fatalf("Kit = %q, want %q", matched[0].Kit, "go-dev")
	}
}

