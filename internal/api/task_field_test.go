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
