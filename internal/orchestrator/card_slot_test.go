package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestDetailOpenSlotChildID_EmptyOrAbsent pins the "no occupant" cases: a
// bare/empty/nil detail blob, and a detail with only closed/dispatched
// children (dispatched children are already accounted for by a live task
// row — see the card-next-step-and-timeline.md §3.2 invariant — not by this
// JSON-only half of the check).
func TestDetailOpenSlotChildID_EmptyOrAbsent(t *testing.T) {
	for _, detail := range []json.RawMessage{
		nil, []byte(""), []byte("null"), []byte("{}"),
		[]byte(`{"children":[{"id":"c1","status":"closed"}]}`),
		[]byte(`{"children":[{"id":"c1","status":"dispatched","task_ref":"t1"}]}`),
	} {
		got, err := orchestrator.DetailOpenSlotChildID(detail)
		if err != nil {
			t.Fatalf("DetailOpenSlotChildID(%s): unexpected error: %v", detail, err)
		}
		if got != "" {
			t.Fatalf("DetailOpenSlotChildID(%s) = %q, want \"\"", detail, got)
		}
	}
}

// TestDetailOpenSlotChildID_FindsOpenOrSpecced pins that an "open" and a
// "specced" child both count as occupying the single work slot.
func TestDetailOpenSlotChildID_FindsOpenOrSpecced(t *testing.T) {
	cases := []struct {
		name   string
		detail json.RawMessage
		want   string
	}{
		{"open", json.RawMessage(`{"children":[{"id":"c1","status":"open"}]}`), "c1"},
		{"specced", json.RawMessage(`{"children":[{"id":"c1","status":"specced"}]}`), "c1"},
		{
			"first occupant wins when legacy data has more than one",
			json.RawMessage(`{"children":[{"id":"c1","status":"closed"},{"id":"c2","status":"open"},{"id":"c3","status":"specced"}]}`),
			"c2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := orchestrator.DetailOpenSlotChildID(c.detail)
			if err != nil {
				t.Fatalf("DetailOpenSlotChildID: unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("DetailOpenSlotChildID = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDetailOpenSlotChildID_MalformedDetail(t *testing.T) {
	if _, err := orchestrator.DetailOpenSlotChildID(json.RawMessage(`not json`)); err == nil {
		t.Fatal("DetailOpenSlotChildID(malformed): expected error, got nil")
	}
}

// TestCountUnresolvedChildren backs the `boid task diagnose-cards` CLI
// diagnostic (card-next-step-and-timeline.md §8): unlike
// DetailOpenSlotChildID (which only needs "is there ANY occupant" for the
// write-time gates), this counts every contender for the slot — JSON
// open/specced children plus live task rows — so a pre-PR-1 card that
// already accumulated more than one can be found and listed.
func TestCountUnresolvedChildren(t *testing.T) {
	cases := []struct {
		name           string
		detail         json.RawMessage
		openChildCount int
		want           int
	}{
		{"empty", nil, 0, 0},
		{"one open json child", json.RawMessage(`{"children":[{"id":"c1","status":"open"}]}`), 0, 1},
		{"one live task row, no json child", nil, 1, 1},
		{
			"legacy violation: two json children plus a live row",
			json.RawMessage(`{"children":[{"id":"c1","status":"open"},{"id":"c2","status":"specced"}]}`),
			1,
			3,
		},
		{"closed/dispatched json children do not double-count beyond their own live row", json.RawMessage(`{"children":[{"id":"c1","status":"closed"},{"id":"c2","status":"dispatched","task_ref":"t2"}]}`), 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := orchestrator.CountUnresolvedChildren(c.detail, c.openChildCount)
			if err != nil {
				t.Fatalf("CountUnresolvedChildren: unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("CountUnresolvedChildren = %d, want %d", got, c.want)
			}
		})
	}
}

func TestCountUnresolvedChildren_MalformedDetail(t *testing.T) {
	if _, err := orchestrator.CountUnresolvedChildren(json.RawMessage(`not json`), 0); err == nil {
		t.Fatal("CountUnresolvedChildren(malformed): expected error, got nil")
	}
}
