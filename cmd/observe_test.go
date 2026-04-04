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

func TestRenderTaskDetail(t *testing.T) {
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

	for _, want := range []string{
		"ID:", "task-abc",
		"Title:", "Status:", "Description:",
		"instructions:",
		"Actions:", "start",
		"Jobs:",
		"exit=0",
		"Output: done",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}

	// running job should not have exit=
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "job-1") && strings.Contains(line, "exit=") {
			t.Errorf("running job should not have exit= field: %q", line)
		}
	}

	// completed job should have exit=0
	foundExit := false
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "job-2") && strings.Contains(line, "exit=0") {
			foundExit = true
		}
	}
	if !foundExit {
		t.Errorf("completed job line missing exit=0\n%s", got)
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
