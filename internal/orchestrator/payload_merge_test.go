package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

func TestMergeDefaultPayload_NilDefault(t *testing.T) {
	request := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"hello"}}}`)
	result, err := orchestrator.MergeDefaultPayload(nil, request)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}
	if string(result) != string(request) {
		t.Fatalf("expected request payload unchanged, got %s", result)
	}
}

func TestMergeDefaultPayload_NilRequest(t *testing.T) {
	defaultPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"default"}}}`)
	result, err := orchestrator.MergeDefaultPayload(defaultPayload, nil)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}
	if string(result) != string(defaultPayload) {
		t.Fatalf("expected default payload unchanged, got %s", result)
	}
}

func TestMergeDefaultPayload_Override(t *testing.T) {
	defaultPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"default"}}}`)
	requestPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"override"}}}`)

	result, err := orchestrator.MergeDefaultPayload(defaultPayload, requestPayload)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var instructions map[string]json.RawMessage
	if err := json.Unmarshal(merged["instructions"], &instructions); err != nil {
		t.Fatalf("unmarshal instructions: %v", err)
	}
	var executor map[string]string
	if err := json.Unmarshal(instructions["executor"], &executor); err != nil {
		t.Fatalf("unmarshal executor: %v", err)
	}
	if executor["message"] != "override" {
		t.Fatalf("expected executor message %q, got %q", "override", executor["message"])
	}
}

func TestMergeDefaultPayload_RoleAddition(t *testing.T) {
	defaultPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"default"}}}`)
	requestPayload := json.RawMessage(`{"instructions":{"reviewer":{"type":"verification","consumer":"claude-code","message":"review"}}}`)

	result, err := orchestrator.MergeDefaultPayload(defaultPayload, requestPayload)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var instructions map[string]json.RawMessage
	if err := json.Unmarshal(merged["instructions"], &instructions); err != nil {
		t.Fatalf("unmarshal instructions: %v", err)
	}
	if instructions["executor"] == nil {
		t.Fatal("expected executor role to exist")
	}
	if instructions["reviewer"] == nil {
		t.Fatal("expected reviewer role to exist")
	}
}

func TestMergeDefaultPayload_RoleDeletion(t *testing.T) {
	defaultPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"default"}}}`)
	requestPayload := json.RawMessage(`{"instructions":{"executor":null}}`)

	result, err := orchestrator.MergeDefaultPayload(defaultPayload, requestPayload)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var instructions map[string]json.RawMessage
	if err := json.Unmarshal(merged["instructions"], &instructions); err != nil {
		t.Fatalf("unmarshal instructions: %v", err)
	}
	if _, exists := instructions["executor"]; exists {
		t.Fatal("expected executor role to be deleted")
	}
}

func TestMergeDefaultPayload_TopLevelNull(t *testing.T) {
	defaultPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"default"}}}`)
	requestPayload := json.RawMessage(`{"instructions":null}`)

	result, err := orchestrator.MergeDefaultPayload(defaultPayload, requestPayload)
	if err != nil {
		t.Fatalf("MergeDefaultPayload() error = %v", err)
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, exists := merged["instructions"]; exists {
		t.Fatal("expected instructions key to be deleted")
	}
}

func TestFilterInstructions_MessageFallback_DefaultMessage(t *testing.T) {
	// message 省略時、InstructionType のデフォルトメッセージが使われること
	payload := json.RawMessage(`{
		"instructions":{
			"executor":{"type":"execution","consumer":"agent-a"}
		}
	}`)
	results := orchestrator.FilterInstructions(payload, orchestrator.InstructionTypeExecution, "agent-a")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Message == "" {
		t.Fatal("expected default message, got empty")
	}
}

func TestFilterInstructions_MessageFallback_ReworkInheritsExecution(t *testing.T) {
	// rework の message 省略時、同 consumer の execution message を引き継ぐこと
	payload := json.RawMessage(`{
		"instructions":{
			"executor":{"type":"execution","consumer":"agent-a","message":"タスクを実装してください"},
			"reworker":{"type":"rework","consumer":"agent-a"}
		}
	}`)
	results := orchestrator.FilterInstructions(payload, orchestrator.InstructionTypeRework, "agent-a")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Message != "タスクを実装してください" {
		t.Fatalf("expected execution message as fallback, got %q", results[0].Message)
	}
}

func TestFilterInstructions_MessageFallback_ReworkDefaultWhenNoExecution(t *testing.T) {
	// execution message もない場合、rework のデフォルトメッセージが使われること
	payload := json.RawMessage(`{
		"instructions":{
			"reworker":{"type":"rework","consumer":"agent-a"}
		}
	}`)
	results := orchestrator.FilterInstructions(payload, orchestrator.InstructionTypeRework, "agent-a")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Message == "" {
		t.Fatal("expected rework default message, got empty")
	}
}

func TestFilterInstructions_ExplicitMessageNotOverridden(t *testing.T) {
	// message が明示されている場合はフォールバックが適用されないこと
	payload := json.RawMessage(`{
		"instructions":{
			"executor":{"type":"execution","consumer":"agent-a","message":"original message"},
			"reworker":{"type":"rework","consumer":"agent-a","message":"custom rework message"}
		}
	}`)
	results := orchestrator.FilterInstructions(payload, orchestrator.InstructionTypeRework, "agent-a")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Message != "custom rework message" {
		t.Fatalf("expected explicit message, got %q", results[0].Message)
	}
}

func TestRawPayload_YAMLUnmarshal(t *testing.T) {
	data := `
name: impl
transition: feedback-loop
default_payload:
  instructions:
    executor:
      type: execution
      consumer: claude-code
      message: "TDD で実装してください。"
`
	var behavior orchestrator.TaskBehavior
	if err := yaml.Unmarshal([]byte(data), &behavior); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	if behavior.Name != "impl" {
		t.Fatalf("expected name %q, got %q", "impl", behavior.Name)
	}
	if behavior.Transition != "feedback-loop" {
		t.Fatalf("expected transition %q, got %q", "feedback-loop", behavior.Transition)
	}

	raw := behavior.DefaultPayload.RawMessage()
	if len(raw) == 0 {
		t.Fatal("expected default_payload to be non-empty")
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal default_payload: %v", err)
	}
	instructions, ok := payload["instructions"]
	if !ok {
		t.Fatal("expected instructions key in default_payload")
	}

	var instructionsMap map[string]json.RawMessage
	if err := json.Unmarshal(instructions, &instructionsMap); err != nil {
		t.Fatalf("unmarshal instructions: %v", err)
	}
	executor, ok := instructionsMap["executor"]
	if !ok {
		t.Fatal("expected executor role in instructions")
	}

	var executorMap map[string]string
	if err := json.Unmarshal(executor, &executorMap); err != nil {
		t.Fatalf("unmarshal executor: %v", err)
	}
	if executorMap["type"] != "execution" {
		t.Fatalf("expected executor type %q, got %q", "execution", executorMap["type"])
	}
	if executorMap["consumer"] != "claude-code" {
		t.Fatalf("expected executor consumer %q, got %q", "claude-code", executorMap["consumer"])
	}
	if executorMap["message"] != "TDD で実装してください。" {
		t.Fatalf("expected executor message %q, got %q", "TDD で実装してください。", executorMap["message"])
	}
}
