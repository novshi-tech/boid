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
			{Type: orchestrator.InstructionTypeExecution, Agent: "claude-code", Message: "do stuff"},
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
			{Type: orchestrator.InstructionTypeExecution, Agent: "claude-code", Message: "do something"},
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
			{Type: orchestrator.InstructionTypeExecution, Agent: "claude-code", Message: "do something"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-codex",
			Kind:     projectspec.HandlerKindAgent,
			Agent: "codex",
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

func TestEvaluateGates_MatchingGate(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":"http://example.com"}`),
	}
	gates := []projectspec.Gate{
		{
			ID:    "push-pr",
			Phase: projectspec.GatePhaseExit,
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	}

	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
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
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	}

	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched gate (status no longer filtered), got %d", len(matched))
	}
}

func TestEvaluate_OptionalTrait_FiresWhenAbsent(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	// executing: verification not in payload yet — hook should still fire
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
		Instructions: orchestrator.Instructions{
			{Type: orchestrator.InstructionTypeExecution, Agent: "claude-code", Message: "impl"},
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

func TestEvaluateGates_EntryOnly(t *testing.T) {
	eval := &orchestrator.Evaluator{}
	gates := []projectspec.Gate{
		{ID: "entry-gate", Phase: projectspec.GatePhaseEntry},
		{ID: "exit-gate", Phase: projectspec.GatePhaseExit},
	}
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting, Payload: json.RawMessage(`{}`)}

	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseEntry)
	if len(matched) != 1 || matched[0].ID != "entry-gate" {
		t.Fatalf("expected only entry-gate, got %v", matched)
	}
}

func TestEvaluateGates_ExitDefault(t *testing.T) {
	eval := &orchestrator.Evaluator{}
	// Phase omitted in YAML would be set to GatePhaseExit by UnmarshalYAML.
	// Here we test the evaluator's phase filter with an explicitly exit gate.
	gates := []projectspec.Gate{
		{ID: "default-gate", Phase: projectspec.GatePhaseExit},
	}
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting, Payload: json.RawMessage(`{}`)}

	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
	if len(matched) != 1 || matched[0].ID != "default-gate" {
		t.Fatalf("expected default-gate matched as exit, got %v", matched)
	}

	// Should not match entry phase
	matched = eval.EvaluateGates(task, gates, projectspec.GatePhaseEntry)
	if len(matched) != 0 {
		t.Fatalf("exit gate should not match entry phase, got %v", matched)
	}
}

func TestEvaluateGates_MixedPhases(t *testing.T) {
	eval := &orchestrator.Evaluator{}
	gates := []projectspec.Gate{
		{ID: "fetch-jira", Phase: projectspec.GatePhaseEntry},
		{ID: "pr-verify", Phase: projectspec.GatePhaseExit},
		{ID: "auto-merge", Phase: projectspec.GatePhaseEntry},
	}

	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting, Payload: json.RawMessage(`{}`)}

	entry := eval.EvaluateGates(task, gates, projectspec.GatePhaseEntry)
	if len(entry) != 2 {
		t.Fatalf("expected 2 entry gates (fetch-jira, auto-merge), got %d: %v", len(entry), entry)
	}
	got := map[string]bool{}
	for _, g := range entry {
		got[g.ID] = true
	}
	if !got["fetch-jira"] || !got["auto-merge"] {
		t.Fatalf("expected fetch-jira and auto-merge in entry, got %v", entry)
	}

	exit := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
	if len(exit) != 1 || exit[0].ID != "pr-verify" {
		t.Fatalf("expected pr-verify for exit, got %v", exit)
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

func TestEvaluateGates_KitIDPreservedInMatchedGate(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	gates := []projectspec.Gate{
		{
			ID:    "go-dev/auto-merge",
			Phase: projectspec.GatePhaseExit,
			Kit:   "go-dev",
		},
	}

	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched gate, got %d", len(matched))
	}
	if matched[0].Kit != "go-dev" {
		t.Fatalf("Kit = %q, want %q", matched[0].Kit, "go-dev")
	}
}
