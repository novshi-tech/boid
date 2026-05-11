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

func TestJobDetail_GateReplayForm(t *testing.T) {
	job := newJobView("completed")
	job.TaskID = "task-id-1"
	job.Role = "gate"
	job.GateID = "go-dev/pr-verify"
	html := renderJobDetail(t, job)
	// gate ID は kit-name/gate-name 形式で '/' を含むため、chi の単一セグメント
	// match に合わせて %2F へエンコードされている必要がある (生のまま埋め込むと
	// サーバ側で 404 になる)。
	want := "/tasks/task-id-1/gates/go-dev%2Fpr-verify/replay"
	if !strings.Contains(html, want) {
		t.Errorf("gate job detail should contain replay form action %q, got: %s", want, html)
	}
	// 念のため、未エスケープ形式は出力に含まれないことも確認しておく。
	unescaped := "/tasks/task-id-1/gates/go-dev/pr-verify/replay"
	if strings.Contains(html, unescaped) {
		t.Errorf("gate job detail should not contain unescaped path %q", unescaped)
	}
}

func TestJobDetail_NoGateReplayForm_NonGateJob(t *testing.T) {
	job := newJobView("completed")
	job.Role = "main"
	html := renderJobDetail(t, job)
	if strings.Contains(html, "/replay") {
		t.Error("non-gate job detail should not contain replay form")
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

func TestJobDetail_GateJobWithRunningTerminal(t *testing.T) {
	job := newJobView("running")
	job.Interactive = true
	job.Role = "gate"
	job.GateID = "go-dev/pr-verify"
	html := renderJobDetail(t, job)

	// gate ID の '/' は %2F へエンコードされる (chi の単一セグメント match 対策)。
	want := "/tasks/task-id-1/gates/go-dev%2Fpr-verify/replay"
	if !strings.Contains(html, want) {
		t.Errorf("gate+interactive running should contain replay form action %q", want)
	}
	// gate job は Interactive であっても Terminal を埋め込まず SSE 分岐に流れる。
	if strings.Contains(html, "boid-terminal") {
		t.Error("gate job should not embed boid-terminal (suppressed for hook/gate jobs)")
	}
	// gate job は Interactive であっても「全画面で開く」リンクを表示しない。
	if strings.Contains(html, "全画面で開く") {
		t.Error("gate job should not show 全画面で開く link (suppressed for hook/gate jobs)")
	}
}

func TestJobDetail_HookJob_InteractiveStale_NoInteractiveMessage(t *testing.T) {
	// DB に残る旧 hook job レコードが interactive=true を持つ場合でも
	// Interactive 向けメッセージが表示されないことを検証する。
	job := newJobView("completed")
	job.Interactive = true
	job.Role = "hook"
	job.HookID = "claude-code/run-agent"
	job.Output = "hook output"
	html := renderJobDetail(t, job)

	if strings.Contains(html, "job-log-note") {
		t.Error("hook job with stale interactive=true should not show interactive note paragraph")
	}
	if strings.Contains(html, "インタラクティブです") {
		t.Error("hook job with stale interactive=true should not show interactive message")
	}
	// SSE 分岐に流れるので静的 pre は出力されること。
	if !strings.Contains(html, `id="job-log"`) {
		t.Error("hook job should show static pre with id=job-log")
	}
	if !strings.Contains(html, "hook output") {
		t.Error("hook job should render existing output")
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
