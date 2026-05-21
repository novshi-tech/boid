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
	// #job-log pre is present for non-interactive jobs in all statuses.
	for _, status := range []string{"running", "pending", "completed", "failed"} {
		html := renderJobDetail(t, newJobView(status))
		if !strings.Contains(html, `id="job-log"`) {
			t.Errorf("status=%s: #job-log pre should always be present for non-interactive jobs", status)
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

func TestJobDetail_NoReplayForm_NonHookJob(t *testing.T) {
	job := newJobView("completed")
	job.Role = "main"
	html := renderJobDetail(t, job)
	if strings.Contains(html, "/replay") {
		t.Error("non-hook job detail should not contain replay form")
	}
}

func TestJobDetail_InteractiveRunning_EmbedsTerminal(t *testing.T) {
	job := newJobView("running")
	job.Interactive = true
	html := renderJobDetail(t, job)

	if !strings.Contains(html, `data-job-id="test-job-id-1234"`) {
		t.Errorf("interactive running job should have boid-terminal div with data-job-id, got: %s", html)
	}
	if !strings.Contains(html, "boid-terminal") {
		t.Error("interactive running job should contain boid-terminal class")
	}
	if !strings.Contains(html, "/jobs/test-job-id-1234/terminal") {
		t.Errorf("interactive running job should have link to /jobs/{id}/terminal, got: %s", html)
	}
	if strings.Contains(html, "EventSource") {
		t.Error("interactive running job should not contain SSE EventSource")
	}
}

func TestJobDetail_InteractiveCompleted_ShowsStaticPre(t *testing.T) {
	job := newJobView("completed")
	job.Interactive = true
	job.Output = "some output"
	html := renderJobDetail(t, job)

	if strings.Contains(html, "boid-terminal-xterm") {
		t.Error("interactive completed job should not embed xterm widget")
	}
	if !strings.Contains(html, `id="job-log"`) {
		t.Error("interactive completed job should show static pre with id=job-log")
	}
	if !strings.Contains(html, "some output") {
		t.Error("interactive completed job should render existing output")
	}
	if !strings.Contains(html, "job-log-note") {
		t.Error("interactive completed job should show note paragraph")
	}
}

func TestJobDetail_NonInteractive_UnchangedSSE(t *testing.T) {
	job := newJobView("running")
	// Interactive defaults to false in newJobView
	html := renderJobDetail(t, job)

	if !strings.Contains(html, "EventSource") {
		t.Error("non-interactive running job should retain EventSource SSE script")
	}
	if !strings.Contains(html, "/log?follow=true") {
		t.Error("non-interactive running job should reference follow=true endpoint")
	}
	if !strings.Contains(html, `id="job-log"`) {
		t.Error("non-interactive running job should have #job-log pre element")
	}
}

func TestJobDetail_HookJob_InteractiveRunning_EmbedsTerminal(t *testing.T) {
	// Phase 1 of fa18b3f: hook job は PTY 上で interactive に動く。 running
	// 中であれば Web UI も xterm.js を埋め込み、 attach できなければならない。
	job := newJobView("running")
	job.Interactive = true
	job.Role = "hook"
	job.HookID = "claude-code/run-agent"
	html := renderJobDetail(t, job)

	if !strings.Contains(html, "boid-terminal") {
		t.Error("interactive running hook job should embed boid-terminal widget")
	}
	if !strings.Contains(html, `data-job-id="test-job-id-1234"`) {
		t.Error("interactive running hook job should have boid-terminal div with data-job-id")
	}
	if !strings.Contains(html, "/jobs/test-job-id-1234/terminal") {
		t.Error("interactive running hook job should link to /jobs/{id}/terminal")
	}
	if strings.Contains(html, "EventSource") {
		t.Error("interactive running hook job should not fall back to SSE EventSource")
	}
}

func TestJobDetail_HookJob_InteractiveCompleted_ShowsStaticPre(t *testing.T) {
	// runtime は完了後 GC されるため、 attach はできない。
	// 静的 pre + ANSI 注意メッセージにフォールバックする。
	job := newJobView("completed")
	job.Interactive = true
	job.Role = "hook"
	job.HookID = "claude-code/run-agent"
	job.Output = "hook output"
	html := renderJobDetail(t, job)

	if strings.Contains(html, "boid-terminal-xterm") {
		t.Error("completed hook job should not embed xterm widget")
	}
	if !strings.Contains(html, "job-log-note") {
		t.Error("completed interactive hook job should show ANSI note paragraph")
	}
	if !strings.Contains(html, "hook output") {
		t.Error("completed interactive hook job should render existing output")
	}
}

func TestJobPageTitle_HookRole_OmitsBracketPrefix(t *testing.T) {
	// DisplayName 未設定時のフォールバック: HandlerID が使われること。
	got := jobPageTitle(&JobContextView{
		ID:        "abc123",
		Role:      "hook",
		HandlerID: "claude-code/run-agent",
	})
	if strings.Contains(got, "[hook]") {
		t.Errorf("hook job title must not contain [hook], got %q", got)
	}
	if !strings.Contains(got, "claude-code/run-agent") {
		t.Errorf("hook job title must contain handler name, got %q", got)
	}
}

func TestJobPageTitle_ExecutorRole_IncludesBracketPrefix(t *testing.T) {
	// DisplayName 未設定時のフォールバック: HandlerID が使われること。
	got := jobPageTitle(&JobContextView{
		ID:        "abc123",
		Role:      "executor",
		HandlerID: "claude-code/run-agent",
	})
	if !strings.Contains(got, "[executor]") {
		t.Errorf("executor job title must contain [executor], got %q", got)
	}
}

func TestJobPageTitle_HookRole_PrefersDisplayName(t *testing.T) {
	got := jobPageTitle(&JobContextView{
		ID:          "abc123",
		Role:        "hook",
		DisplayName: "Format Check",
		HandlerID:   "go-dev/format-check",
	})
	if !strings.Contains(got, "Format Check") {
		t.Errorf("hook job title must contain DisplayName, got %q", got)
	}
	if strings.Contains(got, "go-dev/format-check") {
		t.Errorf("hook job title must not contain HandlerID when DisplayName is set, got %q", got)
	}
}

func TestJobPageTitle_ExecutorRole_PrefersDisplayName(t *testing.T) {
	got := jobPageTitle(&JobContextView{
		ID:          "abc123",
		Role:        "executor",
		DisplayName: "PR Verify",
		HandlerID:   "go-dev/pr-verify",
	})
	if !strings.Contains(got, "PR Verify") {
		t.Errorf("executor job title must contain DisplayName, got %q", got)
	}
	if !strings.Contains(got, "[executor]") {
		t.Errorf("executor job title must retain [executor] prefix, got %q", got)
	}
	if strings.Contains(got, "go-dev/pr-verify") {
		t.Errorf("executor job title must not contain HandlerID when DisplayName is set, got %q", got)
	}
}

func TestJobPageTitle_FallsBackToHandlerID(t *testing.T) {
	// DisplayName が空の場合は HandlerID を使うこと (既存テストのカバー範囲と同じ)。
	got := jobPageTitle(&JobContextView{
		ID:        "abc123",
		Role:      "hook",
		HandlerID: "go-dev/format-check",
	})
	if !strings.Contains(got, "go-dev/format-check") {
		t.Errorf("hook job title must fall back to HandlerID when DisplayName is empty, got %q", got)
	}
}

func TestJobDetail_NoTask_WithProject_BackURL(t *testing.T) {
	job := &JobContextView{
		ID:        "job-cmd-1",
		TaskID:    "",
		ProjectID: "proj-1",
		Status:    "completed",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	html := renderJobDetail(t, job)
	want := `href="/projects/proj-1/commands"`
	if !strings.Contains(html, want) {
		t.Errorf("project-command job back link should be %q, got: %s", want, html)
	}
	if strings.Contains(html, `href="/tasks/"`) {
		t.Error("project-command job must not link to /tasks/ (would 404)")
	}
}

func TestJobDetail_NoTask_NoProject_BackURL(t *testing.T) {
	job := &JobContextView{
		ID:        "job-orphan-1",
		TaskID:    "",
		ProjectID: "",
		Status:    "completed",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	html := renderJobDetail(t, job)
	want := `href="/"`
	if !strings.Contains(html, want) {
		t.Errorf("orphan job back link should be %q, got: %s", want, html)
	}
}

func TestJobDetail_WithTask_BackURL(t *testing.T) {
	job := newJobView("completed")
	html := renderJobDetail(t, job)
	want := `href="/tasks/task-id-1"`
	if !strings.Contains(html, want) {
		t.Errorf("task job back link should be %q, got: %s", want, html)
	}
}
