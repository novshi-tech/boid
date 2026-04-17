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
		Instructions: map[string]orchestrator.Instruction{
			"executor": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "do stuff"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-agent",
			On:       projectspec.OnValues{"executing"},
			Kind:     projectspec.HandlerKindAgent,
			Consumer: "claude-code",
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
			On:   projectspec.OnValues{"executing"},
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
			On: projectspec.OnValues{"executing"},
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
			On: projectspec.OnValues{"executing"},
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
		{orchestrator.TaskStatusVerifying, orchestrator.InstructionTypeVerification},
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
		Status: orchestrator.TaskStatusExecuting,
		Instructions: map[string]orchestrator.Instruction{
			"executor": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "do something"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-claude",
			On:       projectspec.OnValues{"executing"},
			Kind:     projectspec.HandlerKindAgent,
			Consumer: "claude-code",
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
		Status: orchestrator.TaskStatusExecuting,
		Instructions: map[string]orchestrator.Instruction{
			"executor": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "do something"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-codex",
			On:       projectspec.OnValues{"executing"},
			Kind:     projectspec.HandlerKindAgent,
			Consumer: "codex",
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
			On: projectspec.OnValues{"executing"},
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

	// task has execution-type instructions, but status is verifying
	// -> instType=verification, but no verification-type consumer in task.Instructions
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Instructions: map[string]orchestrator.Instruction{
			"executor": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "do something"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-claude",
			On:       projectspec.OnValues{"verifying"},
			Kind:     projectspec.HandlerKindAgent,
			Consumer: "claude-code",
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
			ID:    "push-pr",
			On:    projectspec.OnValues{"executing"},
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
			On: projectspec.OnValues{"executing"},
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	}

	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matched gates, got %d", len(matched))
	}
}

func TestEvaluateGates_OnMultipleValues_MatchesBothStatuses(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	gates := []projectspec.Gate{
		{
			ID:    "pr-verify",
			On:    projectspec.OnValues{"executing", "reworking"},
			Phase: projectspec.GatePhaseExit,
		},
	}

	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusReworking,
	} {
		task := &orchestrator.Task{
			Status:  status,
			Payload: json.RawMessage(`{}`),
		}
		matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
		if len(matched) != 1 {
			t.Errorf("status %q: expected 1 matched gate, got %d", status, len(matched))
		}
	}
}

func TestEvaluate_OptionalTrait_FiresWhenAbsent(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	// executing: verification not in payload yet — hook should still fire
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
		Instructions: map[string]orchestrator.Instruction{
			"exec": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "impl"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-agent",
			On:       projectspec.OnValues{"executing", "reworking"},
			Consumer: "claude-code",
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

func TestEvaluate_OptionalTrait_FiresWhenPresent(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	// reworking: verification present — hook should fire and get it
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"verification":{"pr-verify":{"findings":[{"message":"CI failed","status":"open"}]}}
		}`),
		Instructions: map[string]orchestrator.Instruction{
			"rework": {Type: orchestrator.InstructionTypeRework, Consumer: "claude-code", Message: "fix"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-agent",
			On:       projectspec.OnValues{"executing", "reworking"},
			Consumer: "claude-code",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{"verification?"},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook (optional trait present), got %d", len(matched))
	}
}

func TestEvaluateGates_OnMultipleValues_NoMatchOtherStatus(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	gates := []projectspec.Gate{
		{
			ID:    "pr-verify",
			On:    projectspec.OnValues{"executing", "reworking"},
			Phase: projectspec.GatePhaseExit,
		},
	}
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusDone,
		Payload: json.RawMessage(`{}`),
	}
	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
	if len(matched) != 0 {
		t.Errorf("expected 0 matched gates for done, got %d", len(matched))
	}
}

func TestEvaluateGates_EntryOnly(t *testing.T) {
	eval := &orchestrator.Evaluator{}
	gates := []projectspec.Gate{
		{ID: "entry-gate", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseEntry},
		{ID: "exit-gate", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseExit},
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
		{ID: "default-gate", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseExit},
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
		{ID: "fetch-jira", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseEntry},
		{ID: "pr-verify", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseExit},
		{ID: "auto-merge", On: projectspec.OnValues{"done"}, Phase: projectspec.GatePhaseEntry},
	}

	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting, Payload: json.RawMessage(`{}`)}

	entry := eval.EvaluateGates(task, gates, projectspec.GatePhaseEntry)
	if len(entry) != 1 || entry[0].ID != "fetch-jira" {
		t.Fatalf("expected fetch-jira for entry, got %v", entry)
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
			On:  projectspec.OnValues{"executing"},
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
			On:    projectspec.OnValues{"executing"},
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
