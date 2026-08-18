package components

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestChildrenPreviewLabel(t *testing.T) {
	cases := []struct {
		name     string
		children []orchestrator.TaskTriageChild
		want     string
	}{
		{"none", nil, ""},
		{
			"specced only",
			[]orchestrator.TaskTriageChild{{Status: orchestrator.TaskTriageChildStatusSpecced}},
			"1 specced",
		},
		{
			"open only",
			[]orchestrator.TaskTriageChild{{Status: orchestrator.TaskTriageChildStatusOpen}},
			"1 open",
		},
		{
			"mixed",
			[]orchestrator.TaskTriageChild{
				{Status: orchestrator.TaskTriageChildStatusSpecced},
				{Status: orchestrator.TaskTriageChildStatusSpecced},
				{Status: orchestrator.TaskTriageChildStatusOpen},
			},
			"2 specced, 1 open",
		},
		{
			"only dispatched/closed falls back to a bare count",
			[]orchestrator.TaskTriageChild{
				{Status: orchestrator.TaskTriageChildStatusDispatched},
				{Status: orchestrator.TaskTriageChildStatusClosed},
			},
			"2 children",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := childrenPreviewLabel(c.children)
			if got != c.want {
				t.Errorf("childrenPreviewLabel(%+v) = %q, want %q", c.children, got, c.want)
			}
		})
	}
}

func makeTreeTestTask(id string) *orchestrator.Task {
	return &orchestrator.Task{
		ID:        id,
		Title:     "task " + id,
		Status:    orchestrator.TaskStatusParked,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// TestTaskTreeRow_RendersSuggestionVerbBadgeAndReason is a render test for
// the verb badge + reason line taskTreeRow adds for BD-8 (Opus review
// finding, 2026-08-18: this had no render test at all, so a typo in the
// class name or field used — e.g. "badge-verb-"+item.Suggestion.Action
// instead of .Verb — would have shipped green).
func TestTaskTreeRow_RendersSuggestionVerbBadgeAndReason(t *testing.T) {
	item := TreeItem{
		Task:       makeTreeTestTask("t1"),
		Suggestion: orchestrator.Suggestion{Verb: "wake", Reason: "source event fired"},
	}

	var buf bytes.Buffer
	if err := TaskTree([]TreeItem{item}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `class="badge badge-verb-wake"`) {
		t.Errorf("expected verb badge with class badge-verb-wake, got: %s", html)
	}
	if !strings.Contains(html, `>wake<`) {
		t.Errorf("expected visible verb label \"wake\", got: %s", html)
	}
	if !strings.Contains(html, "source event fired") {
		t.Errorf("expected reason text, got: %s", html)
	}
}

// TestTaskTreeRow_NoSuggestion_RendersNoVerbBadge covers the (overwhelming
// majority) zero-value case: no badge, no reason line, no "badge-verb-"
// class fragment at all.
func TestTaskTreeRow_NoSuggestion_RendersNoVerbBadge(t *testing.T) {
	item := TreeItem{Task: makeTreeTestTask("t1")}

	var buf bytes.Buffer
	if err := TaskTree([]TreeItem{item}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, "badge-verb-") {
		t.Errorf("expected no verb badge for an empty Suggestion, got: %s", html)
	}
}
