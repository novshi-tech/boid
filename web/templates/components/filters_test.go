package components

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestTaskFilters_ActiveOnlyToggle_UncheckedByDefault pins docs/plans/
// webui-detail-list-redesign.md PR-4's replacement for the old 4-tab status
// switcher (§3.5): the 「アクティブのみ」checkbox is UNCHECKED when
// filter.ActiveOnly is false — the default full-state view.
func TestTaskFilters_ActiveOnlyToggle_UncheckedByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskFilters(orchestrator.TaskFilter{}, nil, nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `name="active"`) {
		t.Fatalf("expected an active-only checkbox input, got: %s", html)
	}
	if strings.Contains(html, `name="active" value="1" checked`) {
		t.Errorf("active-only checkbox must be unchecked when ActiveOnly is false, got: %s", html)
	}
}

// TestTaskFilters_ActiveOnlyToggle_CheckedWhenSet pins the positive twin.
func TestTaskFilters_ActiveOnlyToggle_CheckedWhenSet(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskFilters(orchestrator.TaskFilter{ActiveOnly: true}, nil, nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `name="active" value="1" checked`) {
		t.Errorf("active-only checkbox must be checked when ActiveOnly is true, got: %s", html)
	}
}

// TestTaskFilters_NoStatusTabs pins the removal of the old Open/Closed/
// Queue/Parked tab switcher (statusTab/statusTabActive, deleted by PR-4):
// none of their labels or the old hx-vals status-switch pattern survive.
func TestTaskFilters_NoStatusTabs(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskFilters(orchestrator.TaskFilter{}, nil, nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, gone := range []string{"status-tab", "status-tabs", `role="tab"`, "Queue<", "Parked<"} {
		if strings.Contains(html, gone) {
			t.Errorf("expected the old status-tab UI (%q) to be gone, got: %s", gone, html)
		}
	}
}

// TestTaskFilters_StatusHiddenFieldOnlyWhenSet pins that the toolbar no
// longer writes its own status (no tabs left to write one), but still
// preserves an explicit incoming filter.Status (e.g. a bookmarked deep
// link) as a hidden field so it survives the next form submit.
func TestTaskFilters_StatusHiddenFieldOnlyWhenSet(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskFilters(orchestrator.TaskFilter{}, nil, nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), `name="status"`) {
		t.Errorf("expected no hidden status field when filter.Status is empty, got: %s", buf.String())
	}

	buf.Reset()
	if err := TaskFilters(orchestrator.TaskFilter{Status: "parked"}, nil, nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `name="status" value="parked"`) {
		t.Errorf("expected a hidden status field preserving the explicit filter.Status, got: %s", buf.String())
	}
}
