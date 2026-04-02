package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
)

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
