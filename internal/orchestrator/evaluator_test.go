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
			ID:    "run-agent",
			Kind:  projectspec.HandlerKindAgent,
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

// Phase 3-e fallback: with no behavior hooks declared (typical after the
// boid-kits claude-code retirement) the evaluator synthesizes a virtual
// agent-kind hook for the active instruction's agent so the runner-inner-
// child can still hand the job to the HarnessAdapter.
func TestEvaluate_SynthesizesAgentHook_WhenBehaviorHasNone(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Instructions: orchestrator.Instructions{
			{Agent: "claude-code", Message: "go"},
		},
	}

	matched := eval.Evaluate(task, nil)
	if len(matched) != 1 {
		t.Fatalf("expected 1 synthesized hook, got %d", len(matched))
	}
	got := matched[0]
	if got.Kind != projectspec.HandlerKindAgent {
		t.Errorf("synthesized hook Kind = %q, want %q", got.Kind, projectspec.HandlerKindAgent)
	}
	if got.Agent != "claude-code" {
		t.Errorf("synthesized hook Agent = %q, want claude-code", got.Agent)
	}
	if got.ScriptPath != "" {
		t.Errorf("synthesized hook ScriptPath = %q, want empty (adapter builds its own argv)", got.ScriptPath)
	}
	if got.ID == "" {
		t.Error("synthesized hook ID must be non-empty (used as HandlerID for action logging)")
	}
}

// Unknown agent names map to the shell adapter via harnessTypeForAgent; the
// shell adapter cannot run with an empty Argv, so the fallback must NOT
// synthesize a hook in that case — instead the user sees the same "no
// matching hooks" outcome as before, which surfaces the misconfiguration.
func TestEvaluate_DoesNotSynthesize_ForUnknownAgent(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Instructions: orchestrator.Instructions{
			{Agent: "some-future-agent", Message: "go"},
		},
	}

	matched := eval.Evaluate(task, nil)
	if len(matched) != 0 {
		t.Fatalf("expected 0 hooks (unknown agent must not synthesize), got %d", len(matched))
	}
}

// When the behavior already declares an agent-kind hook (for any agent) the
// evaluator must NOT synthesize: the user has explicit hooks in play and the
// active instruction's agent simply didn't match. Synthesizing here would
// fire a duplicate alongside the user-defined hook or override an intentional
// no-op.
func TestEvaluate_DoesNotSynthesize_WhenAgentHookDeclared(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Instructions: orchestrator.Instructions{
			{Agent: "claude-code", Message: "go"},
		},
	}
	hooks := []projectspec.Hook{
		{
			ID:    "run-codex",
			Kind:  projectspec.HandlerKindAgent,
			Agent: "codex", // mismatch with instruction
		},
	}

	matched := eval.Evaluate(task, hooks)
	if len(matched) != 0 {
		t.Fatalf("expected 0 hooks (mismatched agent hook should not trigger synthesis), got %d", len(matched))
	}
}

// A behavior that ships non-agent hooks (e.g. shell-only artifact handlers)
// but no agent hooks should still receive the synthesized agent hook
// alongside its existing non-agent matches — the kit-retirement fallback is
// gated on "no agent hook declared", not "no hooks at all".
func TestEvaluate_Synthesizes_AlongsideNonAgentHook(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":"http://example.com"}`),
		Instructions: orchestrator.Instructions{
			{Agent: "claude-code", Message: "go"},
		},
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
	if len(matched) != 2 {
		t.Fatalf("expected 2 hooks (non-agent + synthesized agent), got %d: %#v", len(matched), matched)
	}
	var sawNonAgent, sawSynth bool
	for _, h := range matched {
		switch h.ID {
		case "handle-artifact":
			sawNonAgent = true
		default:
			if h.Kind == projectspec.HandlerKindAgent && h.Agent == "claude-code" {
				sawSynth = true
			}
		}
	}
	if !sawNonAgent || !sawSynth {
		t.Errorf("expected both non-agent and synthesized agent hook; got non-agent=%v synth=%v", sawNonAgent, sawSynth)
	}
}

// No instructions at all → nothing to synthesize from; the behavior with no
// hooks returns an empty list, as it always has.
func TestEvaluate_DoesNotSynthesize_WhenInstructionsEmpty(t *testing.T) {
	eval := &orchestrator.Evaluator{}

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
	}

	matched := eval.Evaluate(task, nil)
	if len(matched) != 0 {
		t.Fatalf("expected 0 hooks (no instructions, no behavior hooks), got %d", len(matched))
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

