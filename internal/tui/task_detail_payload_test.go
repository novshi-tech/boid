package tui

import (
	"encoding/json"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- extractPayloadSections tests ---

func TestExtractPayloadSections_Empty(t *testing.T) {
	sections := extractPayloadSections(nil)
	if len(sections) != 0 {
		t.Errorf("nil payload: want 0 sections, got %d", len(sections))
	}

	sections = extractPayloadSections(json.RawMessage("null"))
	if len(sections) != 0 {
		t.Errorf("null payload: want 0 sections, got %d", len(sections))
	}

	sections = extractPayloadSections(json.RawMessage("{}"))
	if len(sections) != 0 {
		t.Errorf("empty object: want 0 sections, got %d", len(sections))
	}
}

func TestExtractPayloadSections_KnownOrder(t *testing.T) {
	payload := json.RawMessage(`{
		"tasks": [],
		"verification": {},
		"instructions": {"main": {}},
		"artifacts": {}
	}`)
	sections := extractPayloadSections(payload)
	if len(sections) != 4 {
		t.Fatalf("want 4 sections, got %d", len(sections))
	}
	// Known sections appear in predefined order: instructions, artifacts, verification, tasks
	wantOrder := []string{"instructions", "artifacts", "verification", "tasks"}
	for i, want := range wantOrder {
		if sections[i].key != want {
			t.Errorf("sections[%d].key = %q, want %q", i, sections[i].key, want)
		}
	}
}

func TestExtractPayloadSections_UnknownKeysAlphabetical(t *testing.T) {
	payload := json.RawMessage(`{"zebra": 1, "apple": 2, "mango": 3}`)
	sections := extractPayloadSections(payload)
	if len(sections) != 3 {
		t.Fatalf("want 3 sections, got %d", len(sections))
	}
	wantOrder := []string{"apple", "mango", "zebra"}
	for i, want := range wantOrder {
		if sections[i].key != want {
			t.Errorf("sections[%d].key = %q, want %q", i, sections[i].key, want)
		}
	}
}

func TestExtractPayloadSections_MixedKnownUnknown(t *testing.T) {
	payload := json.RawMessage(`{"custom": {}, "instructions": {}, "zzz": {}}`)
	sections := extractPayloadSections(payload)
	if len(sections) != 3 {
		t.Fatalf("want 3 sections, got %d", len(sections))
	}
	if sections[0].key != "instructions" {
		t.Errorf("first section: want 'instructions', got %q", sections[0].key)
	}
	// "custom" and "zzz" are alphabetical
	if sections[1].key != "custom" {
		t.Errorf("sections[1]: want 'custom', got %q", sections[1].key)
	}
	if sections[2].key != "zzz" {
		t.Errorf("sections[2]: want 'zzz', got %q", sections[2].key)
	}
}

// --- jsonToYAML / yamlToJSON round-trip tests ---

func TestJSONToYAML_SimpleObject(t *testing.T) {
	data := json.RawMessage(`{"foo": "bar", "num": 42}`)
	yamlStr, err := jsonToYAML(data)
	if err != nil {
		t.Fatalf("jsonToYAML error: %v", err)
	}
	if !containsStr(yamlStr, "foo:") {
		t.Error("YAML should contain 'foo:'")
	}
	if !containsStr(yamlStr, "bar") {
		t.Error("YAML should contain 'bar'")
	}
}

func TestYAMLToJSON_RoundTrip(t *testing.T) {
	original := json.RawMessage(`{"key":"value","count":3}`)
	yamlStr, err := jsonToYAML(original)
	if err != nil {
		t.Fatalf("jsonToYAML error: %v", err)
	}
	result, err := yamlToJSON(yamlStr)
	if err != nil {
		t.Fatalf("yamlToJSON error: %v", err)
	}
	// Re-parse both to compare as maps
	var orig, got map[string]any
	if err := json.Unmarshal(original, &orig); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if orig["key"] != got["key"] {
		t.Errorf("key: want %v, got %v", orig["key"], got["key"])
	}
}

func TestYAMLToJSON_InvalidYAML(t *testing.T) {
	_, err := yamlToJSON(":\tinvalid: yaml: [\x00")
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

// --- renderPayload tests ---

func makeDetailWithPayload(payloadJSON string) *api.TaskDetailView {
	return &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "test-task",
			Title:     "Test Task",
			Status:    orchestrator.TaskStatusExecuting,
			Behavior:  "dev",
			Payload:   json.RawMessage(payloadJSON),
			CreatedAt: time.Now(),
		},
	}
}

func TestRenderPayload_EmptyPayload(t *testing.T) {
	detail := makeDetailWithPayload("{}")
	view := renderPayload(detail, 0, 0, 80, 20)
	if !containsStr(view, "payload is empty") {
		t.Error("expected 'payload is empty' for empty payload")
	}
}

func TestRenderPayload_NilDetail(t *testing.T) {
	view := renderPayload(nil, 0, 0, 80, 20)
	if !containsStr(view, "no task data") {
		t.Error("expected 'no task data' for nil detail")
	}
}

func TestRenderPayload_ShowsSections(t *testing.T) {
	detail := makeDetailWithPayload(`{"instructions": {"main": {}}, "artifacts": {}}`)
	view := renderPayload(detail, 0, 0, 80, 20)
	if !containsStr(view, "Sections:") {
		t.Error("expected 'Sections:' header")
	}
	if !containsStr(view, "instructions") {
		t.Error("expected 'instructions' in section list")
	}
	if !containsStr(view, "artifacts") {
		t.Error("expected 'artifacts' in section list")
	}
}

func TestRenderPayload_CursorHighlight(t *testing.T) {
	detail := makeDetailWithPayload(`{"instructions": {}, "artifacts": {}}`)
	// cursor=0 → instructions selected, should show edit hint
	view := renderPayload(detail, 0, 0, 80, 20)
	if !containsStr(view, "edit: e") {
		t.Error("cursor=0: expected 'edit: e' hint for selected section")
	}
}

func TestRenderPayload_PreviewYAML(t *testing.T) {
	detail := makeDetailWithPayload(`{"instructions": {"main": {"type": "execution"}}}`)
	view := renderPayload(detail, 0, 0, 80, 20)
	if !containsStr(view, "Preview") {
		t.Error("expected 'Preview' separator")
	}
	// YAML preview of the instructions section should appear
	if !containsStr(view, "main:") || !containsStr(view, "type:") {
		t.Error("expected YAML preview to show instructions content")
	}
}

// --- Payload tab cursor navigation in TaskDetailScreen ---

func makeDetailWithPayloadForNav(payloadJSON string) *api.TaskDetailView {
	return &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "test-task-id",
			Title:     "Test Task",
			Status:    orchestrator.TaskStatusExecuting,
			Behavior:  "dev",
			Payload:   json.RawMessage(payloadJSON),
			CreatedAt: time.Now(),
		},
	}
}

func TestPayloadTab_CursorJK(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabPayload
	s.detail = makeDetailWithPayloadForNav(`{"instructions": {}, "artifacts": {}, "verification": {}}`)

	if s.payloadCursor != 0 {
		t.Fatalf("initial payloadCursor: want 0, got %d", s.payloadCursor)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.payloadCursor != 1 {
		t.Errorf("after j: want payloadCursor=1, got %d", s.payloadCursor)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.payloadCursor != 2 {
		t.Errorf("after j: want payloadCursor=2, got %d", s.payloadCursor)
	}

	// can't go past last
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.payloadCursor != 2 {
		t.Errorf("j at end: payloadCursor should stay 2, got %d", s.payloadCursor)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.payloadCursor != 1 {
		t.Errorf("after k: want payloadCursor=1, got %d", s.payloadCursor)
	}

	// can't go below 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.payloadCursor != 0 {
		t.Errorf("k at start: payloadCursor should be 0, got %d", s.payloadCursor)
	}
}

func TestPayloadTab_JK_ResetsPreviewScroll(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabPayload
	s.detail = makeDetailWithPayloadForNav(`{"instructions": {}, "artifacts": {}}`)
	s.payloadScroll = 5

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.payloadScroll != 0 {
		t.Errorf("j: expected payloadScroll reset to 0, got %d", s.payloadScroll)
	}
}

func TestPayloadTab_EKey_PushesEditScreen(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabPayload
	s.detail = makeDetailWithPayloadForNav(`{"instructions": {"main": {}}}`)
	s.payloadCursor = 0

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		t.Fatal("e key on payload tab: expected non-nil cmd")
	}
	msg := cmd()
	push, ok := msg.(pushScreenMsg)
	if !ok {
		t.Fatalf("e key on payload tab: expected pushScreenMsg, got %T", msg)
	}
	if _, ok := push.screen.(*PayloadSectionEditScreen); !ok {
		t.Errorf("e key on payload tab: expected *PayloadSectionEditScreen, got %T", push.screen)
	}
}

func TestPayloadTab_EKey_EmptyPayload_NoOp(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabPayload
	s.detail = makeDetailWithPayloadForNav("{}")

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	// Empty payload → no sections → e should be a no-op
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(pushScreenMsg); ok {
			t.Error("e on empty payload: should not push a screen")
		}
	}
}

func TestPayloadTab_EKey_OtherTabs_PushesTaskEditScreen(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabOverview
	s.detail = makeDetailWithPayloadForNav(`{"instructions": {}}`)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		t.Fatal("e key on overview tab: expected non-nil cmd")
	}
	msg := cmd()
	push, ok := msg.(pushScreenMsg)
	if !ok {
		t.Fatalf("e key on overview tab: expected pushScreenMsg, got %T", msg)
	}
	if _, ok := push.screen.(*TaskEditScreen); !ok {
		t.Errorf("e key on overview tab: expected *TaskEditScreen, got %T", push.screen)
	}
}

func TestPayloadTab_ViewRenders(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabPayload
	s.detail = makeDetailWithPayloadForNav(`{"instructions": {"main": {"type": "execution"}}}`)

	view := s.View(80, 30)
	if !containsStr(view, "Sections:") {
		t.Error("payload tab view should contain 'Sections:'")
	}
	if !containsStr(view, "instructions") {
		t.Error("payload tab view should contain 'instructions'")
	}
}
