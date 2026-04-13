package orchestrator_test

import (
	"encoding/json"
	"strings"
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

func TestFilterInstructions_Interactive_PropagatedToRoutedInstruction(t *testing.T) {
	payload := json.RawMessage(`{
		"instructions":{
			"executor":{"type":"execution","consumer":"agent-a","message":"do it","interactive":true},
			"reviewer":{"type":"execution","consumer":"agent-a","message":"check it"}
		}
	}`)
	results := orchestrator.FilterInstructions(payload, orchestrator.InstructionTypeExecution, "agent-a")
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// sorted: "executor" < "reviewer"
	if !results[0].Interactive {
		t.Errorf("executor: expected Interactive=true, got false")
	}
	if results[1].Interactive {
		t.Errorf("reviewer: expected Interactive=false, got true")
	}
}

func TestFilterInstructions_Model_PropagatedToRoutedInstruction(t *testing.T) {
	payload := json.RawMessage(`{
		"instructions":{
			"executor":{"type":"execution","consumer":"agent-a","message":"do it","model":"claude-opus-4-6"},
			"reviewer":{"type":"execution","consumer":"agent-a","message":"check it"}
		}
	}`)
	results := orchestrator.FilterInstructions(payload, orchestrator.InstructionTypeExecution, "agent-a")
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// sorted: "executor" < "reviewer"
	if results[0].Model != "claude-opus-4-6" {
		t.Errorf("executor: expected Model=%q, got %q", "claude-opus-4-6", results[0].Model)
	}
	if results[1].Model != "" {
		t.Errorf("reviewer: expected Model empty, got %q", results[1].Model)
	}
}

func TestRawPayload_YAMLUnmarshal(t *testing.T) {
	data := `
name: impl
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

func TestInstruction_Name_YAMLRoundTrip(t *testing.T) {
	data := `
type: verification
consumer: claude-code
name: security
message: "成果物を検証してください"
`
	var inst orchestrator.Instruction
	if err := yaml.Unmarshal([]byte(data), &inst); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if inst.Name != "security" {
		t.Fatalf("expected Name %q, got %q", "security", inst.Name)
	}
	if inst.Type != orchestrator.InstructionTypeVerification {
		t.Fatalf("expected Type %q, got %q", orchestrator.InstructionTypeVerification, inst.Type)
	}

	out, err := yaml.Marshal(inst)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var roundTripped orchestrator.Instruction
	if err := yaml.Unmarshal(out, &roundTripped); err != nil {
		t.Fatalf("yaml.Unmarshal round-trip: %v", err)
	}
	if roundTripped.Name != "security" {
		t.Fatalf("round-trip Name: expected %q, got %q", "security", roundTripped.Name)
	}
}

func TestInstruction_Name_JSONRoundTrip(t *testing.T) {
	inst := orchestrator.Instruction{
		Type:     orchestrator.InstructionTypeVerification,
		Consumer: "claude-code",
		Name:     "performance",
		Message:  "パフォーマンスを検証してください",
	}

	b, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var roundTripped orchestrator.Instruction
	if err := json.Unmarshal(b, &roundTripped); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if roundTripped.Name != "performance" {
		t.Fatalf("round-trip Name: expected %q, got %q", "performance", roundTripped.Name)
	}
}

func TestInstruction_Name_OmittedWhenEmpty(t *testing.T) {
	inst := orchestrator.Instruction{
		Type:     orchestrator.InstructionTypeExecution,
		Consumer: "claude-code",
		Message:  "タスクを実行してください",
	}

	b, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), `"name"`) {
		t.Errorf("JSON should not include 'name' key when empty, got: %s", b)
	}
}

func TestFilterInstructions_Name_PropagatedToRoutedInstruction(t *testing.T) {
	payload := json.RawMessage(`{
		"instructions":{
			"reviewer_security":{"type":"verification","consumer":"agent-a","name":"security","message":"check security"},
			"reviewer_perf":{"type":"verification","consumer":"agent-a","name":"performance","message":"check performance"}
		}
	}`)
	results := orchestrator.FilterInstructions(payload, orchestrator.InstructionTypeVerification, "agent-a")
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	names := map[string]string{}
	for _, r := range results {
		names[r.Role] = r.Name
	}
	if names["reviewer_security"] != "security" {
		t.Errorf("reviewer_security: expected Name=%q, got %q", "security", names["reviewer_security"])
	}
	if names["reviewer_perf"] != "performance" {
		t.Errorf("reviewer_perf: expected Name=%q, got %q", "performance", names["reviewer_perf"])
	}
}

func TestFilterInstructions_Name_EmptyWhenNotSet(t *testing.T) {
	payload := json.RawMessage(`{
		"instructions":{
			"main":{"type":"execution","consumer":"agent-a","message":"do it"}
		}
	}`)
	results := orchestrator.FilterInstructions(payload, orchestrator.InstructionTypeExecution, "agent-a")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Name != "" {
		t.Errorf("expected empty Name, got %q", results[0].Name)
	}
}
