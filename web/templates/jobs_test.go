package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func renderJobDetail(t *testing.T, job *JobContextView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := JobDetail(job).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func newJobView(status string) *JobContextView {
	return &JobContextView{
		ID:        "test-job-id-1234",
		TaskID:    "task-id-1",
		TaskTitle: "Test Task",
		Status:    status,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

func TestJobDetail_RunningHasScript(t *testing.T) {
	html := renderJobDetail(t, newJobView("running"))
	if !strings.Contains(html, "<script>") {
		t.Error("running job should contain <script>")
	}
	if !strings.Contains(html, "EventSource") {
		t.Error("running job should contain EventSource")
	}
	if !strings.Contains(html, "/log?follow=true") {
		t.Error("running job script should reference follow=true endpoint")
	}
}

func TestJobDetail_PendingHasScript(t *testing.T) {
	html := renderJobDetail(t, newJobView("pending"))
	if !strings.Contains(html, "<script>") {
		t.Error("pending job should contain <script>")
	}
}

func TestJobDetail_CompletedNoScript(t *testing.T) {
	html := renderJobDetail(t, newJobView("completed"))
	if strings.Contains(html, "EventSource") {
		t.Error("completed job must not contain EventSource")
	}
}

func TestJobDetail_FailedNoScript(t *testing.T) {
	html := renderJobDetail(t, newJobView("failed"))
	if strings.Contains(html, "EventSource") {
		t.Error("failed job must not contain EventSource")
	}
}

func TestJobDetail_LogPreAlwaysPresent(t *testing.T) {
	for _, status := range []string{"running", "pending", "completed", "failed"} {
		html := renderJobDetail(t, newJobView(status))
		if !strings.Contains(html, `id="job-log"`) {
			t.Errorf("status=%s: #job-log pre should always be present", status)
		}
	}
}

func TestJobDetail_ExistingOutputRendered(t *testing.T) {
	job := newJobView("completed")
	job.Output = "line1\nline2\n"
	html := renderJobDetail(t, job)
	if !strings.Contains(html, "line1") {
		t.Error("existing output should be rendered in pre")
	}
}
