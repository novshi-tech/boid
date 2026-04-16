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
	defaultPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"c","message":"m"}}}`)
	_, err := orchestrator.MergeDefaultPayload(defaultPayload, nil)
	if err == nil {
		t.Fatal("expected error when default payload contains instructions, got nil")
	}
	if !strings.Contains(err.Error(), "instructions") {
		t.Fatalf("expected error to mention instructions, got %v", err)
	}
}

func TestMergeDefaultPayload_RejectsInstructionsInRequest(t *testing.T) {
	requestPayload := json.RawMessage(`{"instructions":{"executor":{"type":"execution","consumer":"c","message":"m"}}}`)
	_, err := orchestrator.MergeDefaultPayload(nil, requestPayload)
	if err == nil {
		t.Fatal("expected error when request payload contains instructions, got nil")
	}
}

func TestMergeDefaultInstructions_NilDefault(t *testing.T) {
	request := json.RawMessage(`{"executor":{"type":"execution","consumer":"claude-code","message":"hello"}}`)
	result, err := orchestrator.MergeDefaultInstructions(nil, request)
	if err != nil {
		t.Fatalf("MergeDefaultInstructions() error = %v", err)
	}
	if result["executor"].Message != "hello" {
		t.Fatalf("expected executor.message %q, got %q", "hello", result["executor"].Message)
	}
}

func TestMergeDefaultInstructions_NilRequest(t *testing.T) {
	def := map[string]orchestrator.Instruction{
		"executor": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "default"},
	}
	result, err := orchestrator.MergeDefaultInstructions(def, nil)
	if err != nil {
		t.Fatalf("MergeDefaultInstructions() error = %v", err)
	}
	if result["executor"].Message != "default" {
		t.Fatalf("expected default preserved, got %q", result["executor"].Message)
	}
}

func TestMergeDefaultInstructions_RoleOverride(t *testing.T) {
	def := map[string]orchestrator.Instruction{
		"executor": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "default"},
	}
	request := json.RawMessage(`{"executor":{"type":"execution","consumer":"claude-code","message":"override"}}`)
	result, err := orchestrator.MergeDefaultInstructions(def, request)
	if err != nil {
		t.Fatalf("MergeDefaultInstructions() error = %v", err)
	}
	if result["executor"].Message != "override" {
		t.Fatalf("expected override, got %q", result["executor"].Message)
	}
}

func TestMergeDefaultInstructions_RoleAddition(t *testing.T) {
	def := map[string]orchestrator.Instruction{
		"executor": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "default"},
	}
	request := json.RawMessage(`{"reviewer":{"type":"verification","consumer":"claude-code","message":"review"}}`)
	result, err := orchestrator.MergeDefaultInstructions(def, request)
	if err != nil {
		t.Fatalf("MergeDefaultInstructions() error = %v", err)
	}
	if _, ok := result["executor"]; !ok {
		t.Fatal("expected executor to remain")
	}
	if _, ok := result["reviewer"]; !ok {
		t.Fatal("expected reviewer to be added")
	}
}

func TestMergeDefaultInstructions_RoleDeletion(t *testing.T) {
	def := map[string]orchestrator.Instruction{
		"executor": {Type: orchestrator.InstructionTypeExecution, Consumer: "claude-code", Message: "default"},
	}
	request := json.RawMessage(`{"executor":null}`)
	result, err := orchestrator.MergeDefaultInstructions(def, request)
	if err != nil {
		t.Fatalf("MergeDefaultInstructions() error = %v", err)
	}
	if _, exists := result["executor"]; exists {
		t.Fatal("expected executor to be deleted")
	}
}

// parseInstructionsJSON extracts the instructions map from a legacy
// {"instructions":{...}} payload shape used by existing fixtures.
func parseInstructionsJSON(t *testing.T, payload json.RawMessage) map[string]orchestrator.Instruction {
	t.Helper()
	var wrapper struct {
		Instructions map[string]orchestrator.Instruction `json:"instructions"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		t.Fatalf("parseInstructionsJSON: %v", err)
	}
	return wrapper.Instructions
}

func TestFilterInstructions_MessageFallback_DefaultMessage(t *testing.T) {
	// message 省略時、InstructionType のデフォルトメッセージが使われること
	payload := json.RawMessage(`{
		"instructions":{
			"executor":{"type":"execution","consumer":"agent-a"}
		}
	}`)
	results := orchestrator.FilterInstructions(parseInstructionsJSON(t, payload), orchestrator.InstructionTypeExecution, "agent-a")
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
	results := orchestrator.FilterInstructions(parseInstructionsJSON(t, payload), orchestrator.InstructionTypeRework, "agent-a")
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
	results := orchestrator.FilterInstructions(parseInstructionsJSON(t, payload), orchestrator.InstructionTypeRework, "agent-a")
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
	results := orchestrator.FilterInstructions(parseInstructionsJSON(t, payload), orchestrator.InstructionTypeRework, "agent-a")
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
	results := orchestrator.FilterInstructions(parseInstructionsJSON(t, payload), orchestrator.InstructionTypeExecution, "agent-a")
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
	results := orchestrator.FilterInstructions(parseInstructionsJSON(t, payload), orchestrator.InstructionTypeExecution, "agent-a")
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

func TestDefaultInstructions_YAMLUnmarshal(t *testing.T) {
	data := `
name: impl
default_instructions:
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

	executor, ok := behavior.DefaultInstructions["executor"]
	if !ok {
		t.Fatal("expected executor role in default_instructions")
	}
	if executor.Type != orchestrator.InstructionTypeExecution {
		t.Fatalf("expected executor type %q, got %q", orchestrator.InstructionTypeExecution, executor.Type)
	}
	if executor.Consumer != "claude-code" {
		t.Fatalf("expected executor consumer %q, got %q", "claude-code", executor.Consumer)
	}
	if executor.Message != "TDD で実装してください。" {
		t.Fatalf("expected executor message %q, got %q", "TDD で実装してください。", executor.Message)
	}
}

func TestDefaultPayload_YAMLUnmarshal_RejectsInstructions(t *testing.T) {
	// default_payload に instructions キーを書いてパースは成功するが、loader 経由で reject される。
	data := `
name: impl
default_payload:
  instructions:
    executor:
      type: execution
      consumer: claude-code
`
	var behavior orchestrator.TaskBehavior
	if err := yaml.Unmarshal([]byte(data), &behavior); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	// Raw decode は成功する (validation は loader で行う)
	if len(behavior.DefaultPayload) == 0 {
		t.Fatal("expected default_payload to parse")
	}
	// validation 関数が reject することを確認
	if err := orchestrator.ValidateDefaultPayloadNoInstructions(behavior.DefaultPayload); err == nil {
		t.Fatal("expected ValidateDefaultPayloadNoInstructions to reject instructions key")
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
	results := orchestrator.FilterInstructions(parseInstructionsJSON(t, payload), orchestrator.InstructionTypeVerification, "agent-a")
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
	results := orchestrator.FilterInstructions(parseInstructionsJSON(t, payload), orchestrator.InstructionTypeExecution, "agent-a")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Name != "" {
		t.Errorf("expected empty Name, got %q", results[0].Name)
	}
}
