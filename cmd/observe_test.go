package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/apiwire"
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
	detail := &apiwire.TaskDetailView{
		Task: &orchestrator.Task{
			ID:          "task-abc",
			ProjectID:   "proj-1",
			Title:       "Test Task",
			Description: "Some description",
			Status:      orchestrator.TaskStatusExecuting,
			Type:        orchestrator.TaskTypeExecution,
			Exec: &orchestrator.ExecAttrs{
				Behavior: "dev",
				Payload:  payload,
			},
			CreatedAt: time.Unix(0, 0).UTC(),
			UpdatedAt: time.Unix(0, 0).UTC(),
		},
		Actions: []*orchestrator.Action{
			{
				ID:        "act-1",
				Type:      "start",
				Payload:   json.RawMessage(`{"key":"val"}`),
				CreatedAt: time.Unix(0, 0).UTC(),
			},
		},
		Jobs: []*apiwire.Job{
			{
				ID:        "job-1",
				HandlerID: "handler-a",
				Role:      "main",
				Status:    apiwire.JobStatusRunning,
				UpdatedAt: time.Unix(0, 0).UTC(),
			},
			{
				ID:        "job-2",
				HandlerID: "handler-b",
				Role:      "hook",
				Status:    apiwire.JobStatusCompleted,
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

	checks := []string{
		"ID:",
		"task-abc",
		"Title:",
		"Status:",
		"Type:",
		"execution",
		"Behavior:",
		"dev",
		"Description:",
		"instructions:",
		"Actions:",
		"start",
		"Jobs:",
		"exit=0",
		"Output: done",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}

	// running job (job-1) の行には exit= が含まれないこと
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "job-1") && strings.Contains(line, "exit=") {
			t.Errorf("running job line should not contain exit=: %q", line)
		}
	}
}

func TestRenderJobShowsAttachability(t *testing.T) {
	job := &apiwire.Job{
		ID:          "job-1",
		TaskID:      "task-1",
		ProjectID:   "proj-1",
		HandlerID:   "hook-a",
		Role:        "hook",
		RuntimeID:   "runtime-1",
		Interactive: true,
		TTY:         true,
		Status:      apiwire.JobStatusRunning,
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

func TestIsTerminalTaskStatus(t *testing.T) {
	terminal := []orchestrator.TaskStatus{
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
		orchestrator.TaskStatusDropped,
	}
	nonTerminal := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		// card-model-cleanup PR-2: TaskStatusCaptured/TaskStatusTriaged/
		// TaskStatusReady no longer exist as Go constants (folded into
		// "parked" by card machine v2 before this PR). isTerminalTaskStatus
		// (and orchestrator.IsTerminalStatus underneath it) never validates
		// its input against the known-status vocabulary — it is a plain
		// switch on the string value — so these raw literals still exercise
		// genuine behavior: an arbitrary/legacy status string must still be
		// treated as non-terminal.
		orchestrator.TaskStatus("captured"),
		orchestrator.TaskStatus("triaged"),
		orchestrator.TaskStatus("ready"),
	}
	for _, s := range terminal {
		if !isTerminalTaskStatus(s) {
			t.Errorf("isTerminalTaskStatus(%q) = false, want true", s)
		}
	}
	for _, s := range nonTerminal {
		if isTerminalTaskStatus(s) {
			t.Errorf("isTerminalTaskStatus(%q) = true, want false", s)
		}
	}
}
