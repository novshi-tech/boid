package sandbox

import "testing"

// ---- Phase 1 PR-5a (docs/plans/cross-project-issue-triage.md 決定14) ----
//
// `boid task triage` has two mutually exclusive forms; these pin that the
// shim keeps them apart. Silently ignoring a filter flag on the single-task
// form (or a task id on the list form) would let a caller believe it filtered
// a listing that actually returned one row, which is exactly the kind of
// quiet mismatch a read surface backing the sole source of truth cannot have.

func TestParseBoidTaskTriage_SingleTask(t *testing.T) {
	req, err := parseBoidTaskTriage([]string{"t1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpTaskTriageGet {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpTaskTriageGet)
	}
	if req.TaskID != "t1" {
		t.Fatalf("task id = %q, want t1", req.TaskID)
	}
}

func TestParseBoidTaskTriage_ListWithFilters(t *testing.T) {
	req, err := parseBoidTaskTriage([]string{"--list", "--status", "queue_next", "--project-id=p1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpTaskTriageList {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpTaskTriageList)
	}
	if req.Status != "queue_next" || req.ProjectID != "p1" {
		t.Fatalf("filters not parsed: %+v", req)
	}
	if req.TaskID != "" {
		t.Fatalf("task id = %q, want empty for the list form", req.TaskID)
	}
}

func TestParseBoidTaskTriage_Rejects(t *testing.T) {
	cases := map[string][]string{
		"no arguments at all":            {},
		"task id combined with --list":   {"t1", "--list"},
		"filter flag on the single form": {"t1", "--status", "parked"},
		"two task ids":                   {"t1", "t2"},
		"unknown flag":                   {"--list", "--urgency", "now"},
	}
	for name, args := range cases {
		if _, err := parseBoidTaskTriage(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}
