package sandbox

import "testing"

// ---- Phase 1 PR-5a (docs/plans/cross-project-issue-triage.md 決定14),
// renamed from `boid task triage` to `boid card get`/`boid card list` by
// docs/plans/card-model-cleanup.md PR-3 §4 ----
//
// The old single command had two mutually exclusive forms (a task id, or
// --list); the rename splits them into explicit `get`/`list` subcommands
// instead, so there is no longer a "both given" case to reject — each
// subcommand only accepts the arguments that make sense for it.

// TestParseBoidRequest_Card_DispatchesToSubcommands pins the `case "card":`
// dispatch arm in parseBoidRequest itself — every sibling top-level
// subcommand (task identity, task resolve-or-capture, project, ...) has a
// test that drives the full argv through parseBoidRequest, not just the
// leaf parse function. Without this, `boid card get`/`boid card list` (the
// exact argv khi's boid_store.py sends) is only proven to parse correctly
// once dispatched, never that dispatch actually reaches it.
func TestParseBoidRequest_Card_DispatchesToSubcommands(t *testing.T) {
	req, err := parseBoidRequest([]string{"card", "get", "t1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpCardGet {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpCardGet)
	}
	if req.TaskID != "t1" {
		t.Fatalf("task id = %q, want t1", req.TaskID)
	}

	req, err = parseBoidRequest([]string{"card", "list", "--status", "queue_next"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpCardList {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpCardList)
	}
	if req.Status != "queue_next" {
		t.Fatalf("status = %q, want queue_next", req.Status)
	}

	if _, err := parseBoidRequest([]string{"card", "bogus"}); err == nil {
		t.Fatal("unsupported card subcommand: expected an error, got success")
	}
}

func TestParseBoidCardGet(t *testing.T) {
	req, err := parseBoidCardGet([]string{"t1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpCardGet {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpCardGet)
	}
	if req.TaskID != "t1" {
		t.Fatalf("task id = %q, want t1", req.TaskID)
	}
}

func TestParseBoidCardGet_Rejects(t *testing.T) {
	cases := map[string][]string{
		"no arguments at all": {},
		"two task ids":        {"t1", "t2"},
		"unknown flag":        {"--list"},
	}
	for name, args := range cases {
		if _, err := parseBoidCardGet(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}

func TestParseBoidCardList_WithFilters(t *testing.T) {
	req, err := parseBoidCardList([]string{"--status", "queue_next", "--project-id=p1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpCardList {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpCardList)
	}
	if req.Status != "queue_next" || req.ProjectID != "p1" {
		t.Fatalf("filters not parsed: %+v", req)
	}
	if req.TaskID != "" {
		t.Fatalf("task id = %q, want empty for the list form", req.TaskID)
	}
}

func TestParseBoidCardList_NoFilters(t *testing.T) {
	req, err := parseBoidCardList(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Op != BoidOpCardList {
		t.Fatalf("op = %q, want %q", req.Op, BoidOpCardList)
	}
}

func TestParseBoidCardList_Rejects(t *testing.T) {
	cases := map[string][]string{
		"positional task id": {"t1"},
		"unknown flag":       {"--urgency", "now"},
	}
	for name, args := range cases {
		if _, err := parseBoidCardList(args); err == nil {
			t.Errorf("%s: expected an error, got success", name)
		}
	}
}
