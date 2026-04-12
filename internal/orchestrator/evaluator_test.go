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
			On: projectspec.OnValues{"executing"},
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
			On: projectspec.OnValues{"executing"},
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
			On: projectspec.OnValues{"executing"},
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
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"do something"}}}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-claude",
			On: projectspec.OnValues{"executing"},
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
			On: projectspec.OnValues{"executing"},
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

	// payload has execution-type instructions, but status is verifying
	// -> instType=verification, but no verification-type consumer in payload
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"do something"}}}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-claude",
			On: projectspec.OnValues{"verifying"},
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
		Payload: json.RawMessage(`{"instructions":{"exec":{"type":"execution","consumer":"claude-code","message":"impl"}}}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-agent",
			On:       projectspec.OnValues{"executing", "reworking"},
			Consumer: "claude-code",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitInstructions, "verification?"},
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
			"instructions":{"rework":{"type":"rework","consumer":"claude-code","message":"fix"}},
			"verification":{"pr-verify":{"findings":[{"message":"CI failed","status":"open"}]}}
		}`),
	}
	hooks := []projectspec.Hook{
		{
			ID:       "run-agent",
			On:       projectspec.OnValues{"executing", "reworking"},
			Consumer: "claude-code",
			Traits: projectspec.HandlerTraits{
				Consumes: []projectspec.TraitType{projectspec.TraitInstructions, "verification?"},
			},
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook (optional trait present), got %d", len(matched))
	}
}

func TestEvaluate_BehaviorFilter(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "dev",
		Payload:  json.RawMessage(`{}`),
	}
	hooks := []projectspec.Hook{
		{ID: "dev-hook", On: projectspec.OnValues{"executing"}, Behavior: projectspec.BehaviorValues{"dev"}},
		{ID: "ci-hook", On: projectspec.OnValues{"executing"}, Behavior: projectspec.BehaviorValues{"ci"}},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook, got %d", len(matched))
	}
	if matched[0].ID != "dev-hook" {
		t.Fatalf("expected dev-hook, got %s", matched[0].ID)
	}
}

func TestEvaluate_BehaviorFilter_Empty(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "dev",
		Payload:  json.RawMessage(`{}`),
	}
	hooks := []projectspec.Hook{
		{ID: "any-hook", On: projectspec.OnValues{"executing"}},
		{ID: "dev-hook", On: projectspec.OnValues{"executing"}, Behavior: projectspec.BehaviorValues{"dev"}},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 2 {
		t.Fatalf("expected 2 matched hooks (empty behavior matches all), got %d", len(matched))
	}
}

func TestEvaluate_BehaviorFilter_List(t *testing.T) {
	eval := &orchestrator.Evaluator{}
	task := &orchestrator.Task{
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "plan",
		Payload:  json.RawMessage(`{}`),
	}
	hooks := []projectspec.Hook{
		{ID: "plan-hook", On: projectspec.OnValues{"executing"}, Behavior: projectspec.BehaviorValues{"plan", "auto_plan"}},
		{ID: "dev-hook", On: projectspec.OnValues{"executing"}, Behavior: projectspec.BehaviorValues{"dev"}},
	}
	matched := eval.Evaluate(task, hooks)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched hook, got %d", len(matched))
	}
	if matched[0].ID != "plan-hook" {
		t.Fatalf("expected plan-hook, got %s", matched[0].ID)
	}
}

func TestEvaluateGates_BehaviorFilter(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "dev",
		Payload:  json.RawMessage(`{}`),
	}
	gates := []projectspec.Gate{
		{ID: "dev-gate", On: projectspec.OnValues{"executing"}, Behavior: projectspec.BehaviorValues{"dev"}},
		{ID: "ci-gate", On: projectspec.OnValues{"executing"}, Behavior: projectspec.BehaviorValues{"ci"}},
		{ID: "any-gate", On: projectspec.OnValues{"executing"}},
	}

	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseExit)
	if len(matched) != 2 {
		t.Fatalf("expected 2 matched gates (dev-gate + any-gate), got %d", len(matched))
	}
	ids := map[string]bool{}
	for _, g := range matched {
		ids[g.ID] = true
	}
	if !ids["dev-gate"] || !ids["any-gate"] {
		t.Fatalf("expected dev-gate and any-gate, got %v", ids)
	}
}

func TestEvaluateGates_BehaviorFilter_List(t *testing.T) {
	eval := &orchestrator.Evaluator{}
	task := &orchestrator.Task{
		Status:   orchestrator.TaskStatusDone,
		Behavior: "auto_plan",
		Payload:  json.RawMessage(`{}`),
	}
	gates := []projectspec.Gate{
		{ID: "create-subtasks", On: projectspec.OnValues{"done"}, Phase: projectspec.GatePhaseEntry, Behavior: projectspec.BehaviorValues{"plan", "auto_plan"}},
		{ID: "dev-gate", On: projectspec.OnValues{"done"}, Phase: projectspec.GatePhaseEntry, Behavior: projectspec.BehaviorValues{"dev"}},
	}
	matched := eval.EvaluateGates(task, gates, projectspec.GatePhaseEntry)
	if len(matched) != 1 {
		t.Fatalf("expected 1 matched gate, got %d", len(matched))
	}
	if matched[0].ID != "create-subtasks" {
		t.Fatalf("expected create-subtasks, got %s", matched[0].ID)
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
