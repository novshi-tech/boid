package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- AnyFatalFindingOpen ----

func TestAnyFatalFindingOpen_NoFindings_False(t *testing.T) {
	cond := orchestrator.AnyFatalFindingOpen()
	payload := json.RawMessage(`{}`)
	if cond(payload) {
		t.Fatal("expected false when no verification findings")
	}
}

func TestAnyFatalFindingOpen_NormalOpenFinding_False(t *testing.T) {
	cond := orchestrator.AnyFatalFindingOpen()
	payload := json.RawMessage(`{
		"verification":{
			"gate-a":{"source_state":"verifying","findings":[{"message":"issue","status":"open","severity":"normal"}]}
		}
	}`)
	if cond(payload) {
		t.Fatal("expected false for normal severity finding")
	}
}

func TestAnyFatalFindingOpen_FatalOpenFinding_True(t *testing.T) {
	cond := orchestrator.AnyFatalFindingOpen()
	payload := json.RawMessage(`{
		"verification":{
			"gate-a":{"source_state":"verifying","findings":[{"message":"fatal issue","status":"open","severity":"fatal"}]}
		}
	}`)
	if !cond(payload) {
		t.Fatal("expected true for fatal+open finding")
	}
}

func TestAnyFatalFindingOpen_FatalResolvedFinding_False(t *testing.T) {
	cond := orchestrator.AnyFatalFindingOpen()
	payload := json.RawMessage(`{
		"verification":{
			"gate-a":{"source_state":"verifying","findings":[{"message":"was fatal","status":"resolved","severity":"fatal"}]}
		}
	}`)
	if cond(payload) {
		t.Fatal("expected false when fatal finding is resolved")
	}
}

func TestAnyFatalFindingOpen_MixedFindings_TrueWhenFatalOpen(t *testing.T) {
	cond := orchestrator.AnyFatalFindingOpen()
	payload := json.RawMessage(`{
		"verification":{
			"gate-a":{"source_state":"verifying","findings":[
				{"message":"normal issue","status":"open","severity":"normal"},
				{"message":"fatal issue","status":"open","severity":"fatal"}
			]}
		}
	}`)
	if !cond(payload) {
		t.Fatal("expected true when at least one fatal+open finding exists")
	}
}

func TestAnyFatalFindingOpen_SeverityOmitted_False(t *testing.T) {
	// severity が省略された場合は normal 扱い（fatal にならない）
	cond := orchestrator.AnyFatalFindingOpen()
	payload := json.RawMessage(`{
		"verification":{
			"gate-a":{"source_state":"verifying","findings":[{"message":"issue","status":"open"}]}
		}
	}`)
	if cond(payload) {
		t.Fatal("expected false when severity is omitted (treated as normal)")
	}
}

func TestAnyFatalFindingOpen_AcrossMultipleSubkeys(t *testing.T) {
	cond := orchestrator.AnyFatalFindingOpen()
	payload := json.RawMessage(`{
		"verification":{
			"gate-a":{"source_state":"verifying","findings":[{"message":"ok","status":"resolved"}]},
			"gate-b":{"source_state":"reworking","findings":[{"message":"fatal","status":"open","severity":"fatal"}]}
		}
	}`)
	if !cond(payload) {
		t.Fatal("expected true when fatal finding is in a different subkey")
	}
}

// ---- LifecycleReworkCount ----

func TestLifecycleReworkCount_NoLifecycle_Zero(t *testing.T) {
	if got := orchestrator.LifecycleReworkCount(json.RawMessage(`{}`)); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

func TestLifecycleReworkCount_Zero(t *testing.T) {
	payload := json.RawMessage(`{"lifecycle":{"executed":false,"rework_count":0}}`)
	if got := orchestrator.LifecycleReworkCount(payload); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

func TestLifecycleReworkCount_NonZero(t *testing.T) {
	payload := json.RawMessage(`{"lifecycle":{"executed":true,"rework_count":3}}`)
	if got := orchestrator.LifecycleReworkCount(payload); got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
}

func TestLifecycleReworkCount_MalformedLifecycle_Zero(t *testing.T) {
	payload := json.RawMessage(`{"lifecycle":"not-an-object"}`)
	if got := orchestrator.LifecycleReworkCount(payload); got != 0 {
		t.Fatalf("expected 0 for malformed lifecycle, got %d", got)
	}
}
