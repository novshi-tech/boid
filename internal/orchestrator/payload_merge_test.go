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

// DeepMergePayload: nil / 空の base / update を正しく扱う
func TestDeepMergePayload_EmptyInputs(t *testing.T) {
	t.Run("nil base returns update", func(t *testing.T) {
		update := json.RawMessage(`{"a":1}`)
		got, err := orchestrator.DeepMergePayload(nil, update)
		if err != nil {
			t.Fatalf("DeepMergePayload() error = %v", err)
		}
		if string(got) != string(update) {
			t.Errorf("got %s, want %s", got, update)
		}
	})

	t.Run("nil update returns base", func(t *testing.T) {
		base := json.RawMessage(`{"a":1}`)
		got, err := orchestrator.DeepMergePayload(base, nil)
		if err != nil {
			t.Fatalf("DeepMergePayload() error = %v", err)
		}
		if string(got) != string(base) {
			t.Errorf("got %s, want %s", got, base)
		}
	})

	t.Run("both nil returns {}", func(t *testing.T) {
		got, err := orchestrator.DeepMergePayload(nil, nil)
		if err != nil {
			t.Fatalf("DeepMergePayload() error = %v", err)
		}
		if string(got) != "{}" {
			t.Errorf("got %s, want %q", got, "{}")
		}
	})

	t.Run("empty object update returns base", func(t *testing.T) {
		base := json.RawMessage(`{"a":1}`)
		got, err := orchestrator.DeepMergePayload(base, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("DeepMergePayload() error = %v", err)
		}
		if string(got) != string(base) {
			t.Errorf("got %s, want %s", got, base)
		}
	})
}

// DeepMergePayload: nested map は再帰的にマージされ、sibling は保持される
func TestDeepMergePayload_NestedObjectsPreserveSiblings(t *testing.T) {
	base := json.RawMessage(`{"artifact":{"commit":"abc","branch":"main"}}`)
	update := json.RawMessage(`{"artifact":{"pr":{"merged":true}}}`)

	got, err := orchestrator.DeepMergePayload(base, update)
	if err != nil {
		t.Fatalf("DeepMergePayload() error = %v", err)
	}

	var m map[string]map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	art := m["artifact"]
	if art["commit"] != "abc" {
		t.Errorf("commit = %v, want abc (base should be preserved)", art["commit"])
	}
	if art["branch"] != "main" {
		t.Errorf("branch = %v, want main (base should be preserved)", art["branch"])
	}
	pr, ok := art["pr"].(map[string]any)
	if !ok {
		t.Fatalf("pr is not a map: %T %v", art["pr"], art["pr"])
	}
	if pr["merged"] != true {
		t.Errorf("pr.merged = %v, want true", pr["merged"])
	}
}

// DeepMergePayload: update の同名キーは base を上書きする (scalar 同士)
func TestDeepMergePayload_ScalarOverride(t *testing.T) {
	base := json.RawMessage(`{"a":1,"b":"old"}`)
	update := json.RawMessage(`{"b":"new","c":3}`)

	got, err := orchestrator.DeepMergePayload(base, update)
	if err != nil {
		t.Fatalf("DeepMergePayload() error = %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["a"] != float64(1) {
		t.Errorf("a = %v, want 1", m["a"])
	}
	if m["b"] != "new" {
		t.Errorf("b = %v, want new", m["b"])
	}
	if m["c"] != float64(3) {
		t.Errorf("c = %v, want 3", m["c"])
	}
}

// DeepMergePayload: update の null 値は該当キーを削除する
func TestDeepMergePayload_NullDeletesKey(t *testing.T) {
	t.Run("top-level null deletes", func(t *testing.T) {
		base := json.RawMessage(`{"a":1,"b":2}`)
		update := json.RawMessage(`{"a":null}`)

		got, err := orchestrator.DeepMergePayload(base, update)
		if err != nil {
			t.Fatalf("DeepMergePayload() error = %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := m["a"]; ok {
			t.Errorf("a should have been deleted, got %v", m["a"])
		}
		if m["b"] != float64(2) {
			t.Errorf("b = %v, want 2", m["b"])
		}
	})

	t.Run("nested null deletes", func(t *testing.T) {
		base := json.RawMessage(`{"artifact":{"commit":"abc","pr":{"merged":false}}}`)
		update := json.RawMessage(`{"artifact":{"pr":null}}`)

		got, err := orchestrator.DeepMergePayload(base, update)
		if err != nil {
			t.Fatalf("DeepMergePayload() error = %v", err)
		}
		var m map[string]map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		art := m["artifact"]
		if _, ok := art["pr"]; ok {
			t.Errorf("artifact.pr should have been deleted, got %v", art["pr"])
		}
		if art["commit"] != "abc" {
			t.Errorf("artifact.commit = %v, want abc (sibling should survive)", art["commit"])
		}
	})
}

// DeepMergePayload: 配列は置換される (マージされない)
func TestDeepMergePayload_ArraysAreReplaced(t *testing.T) {
	base := json.RawMessage(`{"tags":["a","b"]}`)
	update := json.RawMessage(`{"tags":["c"]}`)

	got, err := orchestrator.DeepMergePayload(base, update)
	if err != nil {
		t.Fatalf("DeepMergePayload() error = %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tags, ok := m["tags"].([]any)
	if !ok {
		t.Fatalf("tags is not a slice: %T", m["tags"])
	}
	if len(tags) != 1 || tags[0] != "c" {
		t.Errorf("tags = %v, want [c] (arrays are replaced, not merged)", tags)
	}
}

// DeepMergePayload: 型不一致の場合、update が勝つ (object → scalar など)
func TestDeepMergePayload_TypeMismatchUpdateWins(t *testing.T) {
	t.Run("object replaced by scalar", func(t *testing.T) {
		base := json.RawMessage(`{"x":{"nested":true}}`)
		update := json.RawMessage(`{"x":"scalar"}`)

		got, err := orchestrator.DeepMergePayload(base, update)
		if err != nil {
			t.Fatalf("DeepMergePayload() error = %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m["x"] != "scalar" {
			t.Errorf("x = %v, want scalar", m["x"])
		}
	})

	t.Run("scalar replaced by object", func(t *testing.T) {
		base := json.RawMessage(`{"x":"scalar"}`)
		update := json.RawMessage(`{"x":{"nested":true}}`)

		got, err := orchestrator.DeepMergePayload(base, update)
		if err != nil {
			t.Fatalf("DeepMergePayload() error = %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		obj, ok := m["x"].(map[string]any)
		if !ok {
			t.Fatalf("x is not a map: %T", m["x"])
		}
		if obj["nested"] != true {
			t.Errorf("x.nested = %v, want true", obj["nested"])
		}
	})
}

// DeepMergePayload: 3 段階以上のネストも再帰的にマージされる
func TestDeepMergePayload_DeepNesting(t *testing.T) {
	base := json.RawMessage(`{"a":{"b":{"c":{"x":1,"y":2}}}}`)
	update := json.RawMessage(`{"a":{"b":{"c":{"y":20,"z":30}}}}`)

	got, err := orchestrator.DeepMergePayload(base, update)
	if err != nil {
		t.Fatalf("DeepMergePayload() error = %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	a := m["a"].(map[string]any)
	b := a["b"].(map[string]any)
	c := b["c"].(map[string]any)
	if c["x"] != float64(1) {
		t.Errorf("a.b.c.x = %v, want 1", c["x"])
	}
	if c["y"] != float64(20) {
		t.Errorf("a.b.c.y = %v, want 20", c["y"])
	}
	if c["z"] != float64(30) {
		t.Errorf("a.b.c.z = %v, want 30", c["z"])
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
