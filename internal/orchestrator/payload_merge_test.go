package orchestrator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

// Phase 3-1: MergeDefaultPayload and ValidateDefaultPayloadNoInstructions have
// been removed along with TaskBehavior.DefaultPayload / BehaviorSpec.DefaultPayload.
// The remaining tests cover the surviving payload helpers
// (RejectPayloadInstructions, RejectReservedPayloadKeys, MergePayloadPatch).

func TestRejectPayloadInstructions_Empty_OK(t *testing.T) {
	if err := orchestrator.RejectPayloadInstructions(json.RawMessage(`{}`)); err != nil {
		t.Fatalf("RejectPayloadInstructions({}) error = %v", err)
	}
}

func TestRejectPayloadInstructions_HasInstructions_Errors(t *testing.T) {
	payload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","agent":"c","message":"m"}}}`)
	err := orchestrator.RejectPayloadInstructions(payload)
	if err == nil {
		t.Fatal("expected error when payload contains instructions, got nil")
	}
	if !strings.Contains(err.Error(), "instructions") {
		t.Fatalf("expected error to mention instructions, got %v", err)
	}
}

func TestDefaultInstruction_YAMLUnmarshal(t *testing.T) {
	data := `
default_instruction:
  type: execution
  agent: claude-code
  message: "TDD で実装してください。"
`
	var behavior orchestrator.TaskBehavior
	if err := yaml.Unmarshal([]byte(data), &behavior); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	if behavior.DefaultInstruction == nil {
		t.Fatal("expected default_instruction to be set")
	}
	got := behavior.DefaultInstruction
	if got.Type != orchestrator.InstructionTypeExecution {
		t.Fatalf("expected type %q, got %q", orchestrator.InstructionTypeExecution, got.Type)
	}
	if got.Agent != "claude-code" {
		t.Fatalf("expected agent %q, got %q", "claude-code", got.Agent)
	}
	if got.Message != "TDD で実装してください。" {
		t.Fatalf("expected message %q, got %q", "TDD で実装してください。", got.Message)
	}
}

func TestInstruction_Name_OmittedWhenEmpty(t *testing.T) {
	inst := orchestrator.Instruction{
		Type:    orchestrator.InstructionTypeExecution,
		Agent:   "claude-code",
		Message: "タスクを実行してください",
	}

	b, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), `"name"`) {
		t.Errorf("JSON should not include 'name' key when empty, got: %s", b)
	}
}

// --- RejectReservedPayloadKeys ---

func TestRejectReservedPayloadKeys_ArtifactChildren_ReturnsError(t *testing.T) {
	payload := json.RawMessage(`{"artifact":{"children":{"all_done":true}}}`)
	if err := orchestrator.RejectReservedPayloadKeys(payload); err == nil {
		t.Error("want error for artifact.children.* write, got nil")
	}
}

func TestRejectReservedPayloadKeys_ArtifactOther_OK(t *testing.T) {
	payload := json.RawMessage(`{"artifact":{"auto-merge":{"merged":true}}}`)
	if err := orchestrator.RejectReservedPayloadKeys(payload); err != nil {
		t.Errorf("want nil for non-reserved artifact key, got %v", err)
	}
}

func TestRejectReservedPayloadKeys_Empty_OK(t *testing.T) {
	if err := orchestrator.RejectReservedPayloadKeys(json.RawMessage(`{}`)); err != nil {
		t.Errorf("want nil for empty payload, got %v", err)
	}
}

func TestRejectReservedPayloadKeys_NoArtifact_OK(t *testing.T) {
	payload := json.RawMessage(`{"other":{"key":"value"}}`)
	if err := orchestrator.RejectReservedPayloadKeys(payload); err != nil {
		t.Errorf("want nil for non-artifact key, got %v", err)
	}
}

func TestMergePayloadPatch_ArtifactChildren_ReturnsError(t *testing.T) {
	base := json.RawMessage(`{}`)
	patch := json.RawMessage(`{"artifact":{"children":{"all_done":true}}}`)
	_, err := orchestrator.MergePayloadPatch(base, patch, "test-handler", nil)
	if err == nil {
		t.Error("want error for artifact.children.* in patch, got nil")
	}
}
