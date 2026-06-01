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
	if got.Agent != "claude-code" {
		t.Fatalf("expected agent %q, got %q", "claude-code", got.Agent)
	}
	if got.Message != "TDD で実装してください。" {
		t.Fatalf("expected message %q, got %q", "TDD で実装してください。", got.Message)
	}
}

func TestInstruction_Name_OmittedWhenEmpty(t *testing.T) {
	inst := orchestrator.Instruction{
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

// --- MergeDefaultInstructions ---

func TestMergeDefaultInstructions_NoDefault_OverrideUsedAsIs(t *testing.T) {
	raw := json.RawMessage(`[{"type":"execution","agent":"claude-code","model":"claude-opus-4-7"}]`)
	got, err := orchestrator.MergeDefaultInstructions(nil, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 instruction, got %d", len(got))
	}
	if got[0].Model != "claude-opus-4-7" {
		t.Errorf("model: want claude-opus-4-7, got %q", got[0].Model)
	}
}

func TestMergeDefaultInstructions_EmptyOverride_ReturnsDefault(t *testing.T) {
	def := &orchestrator.Instruction{
		Agent:   "claude-code",
		Message: "default message",
		Model:   "claude-sonnet-4-6",
	}
	got, err := orchestrator.MergeDefaultInstructions(def, json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 instruction, got %d", len(got))
	}
	if got[0].Model != "claude-sonnet-4-6" {
		t.Errorf("model: want claude-sonnet-4-6, got %q", got[0].Model)
	}
	if got[0].Message != "default message" {
		t.Errorf("message: want %q, got %q", "default message", got[0].Message)
	}
}

func TestMergeDefaultInstructions_SingleOverride_ModelOnly(t *testing.T) {
	// override only sets model; other fields should inherit from default.
	def := &orchestrator.Instruction{
		Agent:   "claude-code",
		Message: "do the thing",
		Model:   "claude-sonnet-4-6",
	}
	raw := json.RawMessage(`[{"model":"claude-opus-4-7"}]`)
	got, err := orchestrator.MergeDefaultInstructions(def, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 instruction, got %d", len(got))
	}
	if got[0].Model != "claude-opus-4-7" {
		t.Errorf("model: want claude-opus-4-7, got %q", got[0].Model)
	}
	if got[0].Message != "do the thing" {
		t.Errorf("message should be inherited: want %q, got %q", "do the thing", got[0].Message)
	}
	if got[0].Agent != "claude-code" {
		t.Errorf("agent should be inherited: want claude-code, got %q", got[0].Agent)
	}
}

func TestMergeDefaultInstructions_SingleOverride_MessageOnly(t *testing.T) {
	// override only replaces message; model etc. come from default.
	def := &orchestrator.Instruction{
		Agent:   "claude-code",
		Message: "original",
		Model:   "claude-sonnet-4-6",
	}
	raw := json.RawMessage(`[{"message":"replacement"}]`)
	got, err := orchestrator.MergeDefaultInstructions(def, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Message != "replacement" {
		t.Errorf("message: want replacement, got %q", got[0].Message)
	}
	if got[0].Model != "claude-sonnet-4-6" {
		t.Errorf("model should be inherited: want claude-sonnet-4-6, got %q", got[0].Model)
	}
}

func TestMergeDefaultInstructions_SingleOverride_AllFields_NoInheritance(t *testing.T) {
	// override fills every field → result equals override (no inheritance needed).
	def := &orchestrator.Instruction{
		Agent:   "claude-code",
		Message: "original message",
		Model:   "claude-sonnet-4-6",
	}
	raw := json.RawMessage(`[{"type":"execution","agent":"claude-code","message":"override message","model":"claude-opus-4-7"}]`)
	got, err := orchestrator.MergeDefaultInstructions(def, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Message != "override message" {
		t.Errorf("message: want override message, got %q", got[0].Message)
	}
	if got[0].Model != "claude-opus-4-7" {
		t.Errorf("model: want claude-opus-4-7, got %q", got[0].Model)
	}
}

func TestMergeDefaultInstructions_MultipleOverride_CompleteReplacement(t *testing.T) {
	// 2 entries → full replacement, default is ignored.
	def := &orchestrator.Instruction{
		Agent:   "claude-code",
		Message: "default",
		Model:   "claude-sonnet-4-6",
	}
	raw := json.RawMessage(`[{"type":"execution","agent":"claude-code","message":"first"},{"type":"execution","agent":"claude-code","message":"second"}]`)
	got, err := orchestrator.MergeDefaultInstructions(def, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 instructions (full replacement), got %d", len(got))
	}
	if got[0].Message != "first" {
		t.Errorf("first message: want first, got %q", got[0].Message)
	}
	if got[1].Message != "second" {
		t.Errorf("second message: want second, got %q", got[1].Message)
	}
}
