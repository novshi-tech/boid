package orchestrator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

func TestMergeDefaultPayload_NilDefault(t *testing.T) {
	request := json.RawMessage(`{"artifact":{"url":"example"}}`)
	result, err := orchestrator.MergeDefaultPayload(nil, request)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}
	if string(result) != string(request) {
		t.Fatalf("expected request payload unchanged, got %s", result)
	}
}

func TestMergeDefaultPayload_NilRequest(t *testing.T) {
	defaultPayload := json.RawMessage(`{"artifact":{"url":"default"}}`)
	result, err := orchestrator.MergeDefaultPayload(defaultPayload, nil)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}
	if string(result) != string(defaultPayload) {
		t.Fatalf("expected default payload unchanged, got %s", result)
	}
}

func TestMergeDefaultPayload_Override(t *testing.T) {
	defaultPayload := json.RawMessage(`{"artifact":{"url":"default"}}`)
	requestPayload := json.RawMessage(`{"artifact":{"url":"override"}}`)

	result, err := orchestrator.MergeDefaultPayload(defaultPayload, requestPayload)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var artifact map[string]string
	if err := json.Unmarshal(merged["artifact"], &artifact); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}
	if artifact["url"] != "override" {
		t.Fatalf("expected artifact url %q, got %q", "override", artifact["url"])
	}
}

func TestMergeDefaultPayload_RejectsInstructionsInDefault(t *testing.T) {
	defaultPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","agent":"c","message":"m"}}}`)
	_, err := orchestrator.MergeDefaultPayload(defaultPayload, nil)
	if err == nil {
		t.Fatal("expected error when default payload contains instructions, got nil")
	}
	if !strings.Contains(err.Error(), "instructions") {
		t.Fatalf("expected error to mention instructions, got %v", err)
	}
}

func TestMergeDefaultPayload_RejectsInstructionsInRequest(t *testing.T) {
	requestPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","agent":"c","message":"m"}}}`)
	_, err := orchestrator.MergeDefaultPayload(nil, requestPayload)
	if err == nil {
		t.Fatal("expected error when request payload contains instructions, got nil")
	}
}

func TestDefaultInstruction_YAMLUnmarshal(t *testing.T) {
	data := `
name: impl
default_instruction:
  type: execution
  agent: claude-code
  message: "TDD で実装してください。"
`
	var behavior orchestrator.TaskBehavior
	if err := yaml.Unmarshal([]byte(data), &behavior); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	if behavior.Name != "impl" {
		t.Fatalf("expected name %q, got %q", "impl", behavior.Name)
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

func TestDefaultPayload_YAMLUnmarshal_RejectsInstructions(t *testing.T) {
	data := `
name: impl
default_payload:
  instructions:
    executor:
      type: execution
      agent: claude-code
`
	var behavior orchestrator.TaskBehavior
	if err := yaml.Unmarshal([]byte(data), &behavior); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if len(behavior.DefaultPayload) == 0 {
		t.Fatal("expected default_payload to parse")
	}
	if err := orchestrator.ValidateDefaultPayloadNoInstructions(behavior.DefaultPayload); err == nil {
		t.Fatal("expected ValidateDefaultPayloadNoInstructions to reject instructions key")
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
