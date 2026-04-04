package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatalf("read output: %v", err)
	}
	return out.String()
}

func TestRenderTaskDetailYAML(t *testing.T) {
	payload := json.RawMessage(`{"instructions":{"main":{"type":"execution"}}}`)
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:          "task-abc",
			ProjectID:   "proj-1",
			Title:       "Test Task",
			Description: "Some description",
			Status:      orchestrator.TaskStatusExecuting,
			Behavior:    "dev",
			Payload:     payload,
			CreatedAt:   time.Unix(0, 0).UTC(),
			UpdatedAt:   time.Unix(0, 0).UTC(),
		},
		Actions: []*orchestrator.Action{
			{
				ID:        "act-1",
				Type:      "start",
				Payload:   json.RawMessage(`{"key":"val"}`),
				CreatedAt: time.Unix(0, 0).UTC(),
			},
		},
		Jobs: []*api.Job{
			{
				ID:        "job-1",
				HandlerID: "handler-a",
				Role:      "main",
				Status:    api.JobStatusRunning,
				UpdatedAt: time.Unix(0, 0).UTC(),
			},
			{
				ID:        "job-2",
				HandlerID: "handler-b",
				Role:      "hook",
				Status:    api.JobStatusCompleted,
				ExitCode:  0,
				Output:    "done",
				UpdatedAt: time.Unix(0, 0).UTC(),
			},
		},
	}

	got := captureStdout(t, func() {
		if err := renderTaskDetail(detail); err != nil {
			t.Fatalf("renderTaskDetail: %v", err)
		}
	})

	// must be valid YAML
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output is not valid YAML: %v\n%s", err, got)
	}

	// must not contain "Key:   Value" style text lines
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "ID:") || strings.HasPrefix(line, "Project:") || strings.HasPrefix(line, "Title:") {
			t.Errorf("output contains legacy text format line: %q", line)
		}
	}

	if parsed["id"] != "task-abc" {
		t.Errorf("id: want task-abc, got %v", parsed["id"])
	}
	if parsed["status"] != "executing" {
		t.Errorf("status: want executing, got %v", parsed["status"])
	}
	if parsed["description"] != "Some description" {
		t.Errorf("description: want 'Some description', got %v", parsed["description"])
	}

	actions, ok := parsed["actions"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("actions: expected 1 entry, got %v", parsed["actions"])
	}
	act := actions[0].(map[string]any)
	if act["type"] != "start" {
		t.Errorf("action type: want start, got %v", act["type"])
	}

	jobs, ok := parsed["jobs"].([]any)
	if !ok || len(jobs) != 2 {
		t.Fatalf("jobs: expected 2 entries, got %v", parsed["jobs"])
	}
	// running job should have no exit_code
	j0 := jobs[0].(map[string]any)
	if _, exists := j0["exit_code"]; exists {
		t.Errorf("running job should not have exit_code field")
	}
	// completed job should have exit_code
	j1 := jobs[1].(map[string]any)
	if _, exists := j1["exit_code"]; !exists {
		t.Errorf("completed job should have exit_code field")
	}
	if j1["output"] != "done" {
		t.Errorf("job output: want done, got %v", j1["output"])
	}
}

func TestRenderJobShowsAttachability(t *testing.T) {
	job := &api.Job{
		ID:          "job-1",
		TaskID:      "task-1",
		ProjectID:   "proj-1",
		HandlerID:   "hook-a",
		Role:        "hook",
		RuntimeID:   "runtime-1",
		Interactive: true,
		TTY:         true,
		Status:      api.JobStatusRunning,
		CreatedAt:   time.Unix(0, 0).UTC(),
		UpdatedAt:   time.Unix(0, 0).UTC(),
	}

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	renderJob(job)

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatalf("read output: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Runtime:    runtime-1") {
		t.Fatalf("renderJob output missing runtime id: %q", got)
	}
	if !strings.Contains(got, "Attachable: yes") {
		t.Fatalf("renderJob output missing attachability: %q", got)
	}
	if !strings.Contains(got, "TTY:        yes") {
		t.Fatalf("renderJob output missing tty flag: %q", got)
	}
}
