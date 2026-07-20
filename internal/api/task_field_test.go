package api

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type stubLifecycleStore struct {
	actions []*orchestrator.Action
}

func (s *stubLifecycleStore) ListActionsByTask(_ string) ([]*orchestrator.Action, error) {
	return s.actions, nil
}

func TestResolveTaskField_TopLevel(t *testing.T) {
	task := &orchestrator.Task{
		ID:          "task-1",
		Title:       "hello",
		Description: "world",
		Status:      orchestrator.TaskStatusExecuting,
		ParentID:    "parent-1",
	}

	cases := []struct {
		path string
		want string
	}{
		{"id", "task-1"},
		{"title", "hello"},
		{"description", "world"},
		{"status", "executing"},
		{"parent_id", "parent-1"},
	}
	for _, c := range cases {
		got, err := ResolveTaskField(task, nil, c.path)
		if err != nil {
			t.Fatalf("path %q: unexpected error: %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("path %q: got %q, want %q", c.path, got, c.want)
		}
	}
}

func TestResolveTaskField_PayloadExplicitPrefix(t *testing.T) {
	payload := json.RawMessage(`{"artifact":{"report":{"summary":"ok"}}}`)
	task := &orchestrator.Task{ID: "t", Payload: payload}

	got, err := ResolveTaskField(task, nil, "payload.artifact.report.summary")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

func TestResolveTaskField_PayloadAutoFallback(t *testing.T) {
	payload := json.RawMessage(`{"awaiting":{"question":"why?","question_id":"q1"},"artifact":{"report":"done"}}`)
	task := &orchestrator.Task{ID: "t", Payload: payload}

	cases := []struct {
		path string
		want string
	}{
		{"awaiting.question", "why?"},
		{"awaiting.question_id", "q1"},
		{"artifact.report", "done"},
	}
	for _, c := range cases {
		got, err := ResolveTaskField(task, nil, c.path)
		if err != nil {
			t.Fatalf("path %q: unexpected error: %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("path %q: got %q, want %q", c.path, got, c.want)
		}
	}
}

func TestResolveTaskField_PayloadWhole(t *testing.T) {
	payload := json.RawMessage(`{"k":"v"}`)
	task := &orchestrator.Task{ID: "t", Payload: payload}

	got, err := ResolveTaskField(task, nil, "payload")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `{"k":"v"}` {
		t.Errorf("got %q, want %q", got, `{"k":"v"}`)
	}
}

func TestResolveTaskField_LifecycleDerived(t *testing.T) {
	abortPayload, _ := json.Marshal(map[string]string{
		"code":    "user_aborted",
		"message": "stopped by user",
	})
	store := &stubLifecycleStore{
		actions: []*orchestrator.Action{
			{
				TaskID:   "t",
				ToStatus: orchestrator.TaskStatusAborted,
				Payload:  abortPayload,
			},
		},
	}

	task := &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusAborted}

	cases := []struct {
		path string
		want string
	}{
		{"lifecycle.abort.message", "stopped by user"},
		{"lifecycle.abort.code", "user_aborted"},
		{"payload.lifecycle.abort.message", "stopped by user"},
		{"lifecycle.executed", "false"},
	}
	for _, c := range cases {
		got, err := ResolveTaskField(task, store, c.path)
		if err != nil {
			t.Fatalf("path %q: unexpected error: %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("path %q: got %q, want %q", c.path, got, c.want)
		}
	}
}

func TestResolveTaskField_LifecycleWithoutStore(t *testing.T) {
	task := &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusAborted}

	got, err := ResolveTaskField(task, nil, "lifecycle.abort.message")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (lifecycle unavailable without store)", got)
	}
}

func TestResolveTaskField_MissingPathReturnsEmpty(t *testing.T) {
	task := &orchestrator.Task{ID: "t", Payload: json.RawMessage(`{}`)}

	got, err := ResolveTaskField(task, nil, "awaiting.question")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveTaskField_EmptyPath(t *testing.T) {
	task := &orchestrator.Task{ID: "t"}
	if _, err := ResolveTaskField(task, nil, ""); err == nil {
		t.Errorf("expected error for empty path")
	}
}

func TestResolveTaskField_TraverseScalar(t *testing.T) {
	payload := json.RawMessage(`{"k":"v"}`)
	task := &orchestrator.Task{ID: "t", Payload: payload}

	if _, err := ResolveTaskField(task, nil, "title.foo"); err == nil {
		t.Errorf("expected error when traversing into scalar")
	}
}

// --- ResolveJSONField (Phase 5b PR1 task-context RPCs share this generic core) ---

func TestResolveJSONField_TopLevel(t *testing.T) {
	raw := json.RawMessage(`{"id":"t1","title":"hello","behavior":"dev"}`)
	got, err := ResolveJSONField(raw, "title")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestResolveJSONField_Nested(t *testing.T) {
	raw := json.RawMessage(`{"allowed_domains":["a.com"],"host_commands":[{"name":"gh","allow":["pr"]}]}`)
	got, err := ResolveJSONField(raw, "host_commands.0.name")
	// Array indices are not object keys — traversing "0" into an array falls
	// through the map[string]any case's default branch, matching
	// ResolveTaskField's existing "cannot traverse into non-object" behavior
	// for arrays (arrays are consumed whole, not indexed).
	if err == nil {
		t.Fatalf("expected error traversing into an array by numeric segment, got %q", got)
	}
}

func TestResolveJSONField_WholeArrayValue(t *testing.T) {
	raw := json.RawMessage(`{"allowed_domains":["a.com","b.com"]}`)
	got, err := ResolveJSONField(raw, "allowed_domains")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `["a.com","b.com"]` {
		t.Errorf("got %q, want compact JSON array", got)
	}
}

func TestResolveJSONField_MissingPathReturnsEmpty(t *testing.T) {
	raw := json.RawMessage(`{}`)
	got, err := ResolveJSONField(raw, "not.there")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveJSONField_EmptyPath(t *testing.T) {
	if _, err := ResolveJSONField(json.RawMessage(`{}`), ""); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestResolveJSONField_InvalidJSON(t *testing.T) {
	if _, err := ResolveJSONField(json.RawMessage(`not json`), "x"); err == nil {
		t.Error("expected error for invalid JSON input")
	}
}

func TestResolveJSONField_NoTaskSpecificPayloadPrefixing(t *testing.T) {
	// Unlike ResolveTaskField, a top-level segment that doesn't exist must
	// NOT be silently re-tried under an implicit "payload." prefix.
	raw := json.RawMessage(`{"payload":{"awaiting":{"question":"why?"}}}`)
	got, err := ResolveJSONField(raw, "awaiting.question")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (no implicit payload prefix)", got)
	}
}
