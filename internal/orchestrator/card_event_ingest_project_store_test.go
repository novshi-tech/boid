package orchestrator_test

// Proves *ProjectStore.CardEventCommand actually reads card_events.command
// off a real project.yaml through the ordinary ProjectStore.Load path —
// ProjectMeta.CardEvents carries json:"-" (API-serialization concern only),
// which does not affect this in-process read.

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestProjectStore_CardEventCommand_FromRealProjectYAML(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
card_commands:
  discuss:
    label: Discuss
    run: python3 scripts/card_discuss.py
  review:
    label: Run
    run: python3 scripts/card_review.py
card_events:
  command: review
`)

	s := orchestrator.NewProjectStore()
	if _, err := s.Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, ok := s.CardEventCommand("test-proj")
	if !ok {
		t.Fatal("CardEventCommand: ok = false, want true")
	}
	if got != "review" {
		t.Fatalf("CardEventCommand = %q, want %q", got, "review")
	}
}

func TestProjectStore_CardEventCommand_NoCardEventsDeclared_NotOK(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
`)

	s := orchestrator.NewProjectStore()
	if _, err := s.Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if _, ok := s.CardEventCommand("test-proj"); ok {
		t.Fatal("CardEventCommand: ok = true, want false (no card_events declared)")
	}
}

func TestProjectStore_CardEventCommand_UnknownProject_NotOK(t *testing.T) {
	s := orchestrator.NewProjectStore()
	if _, ok := s.CardEventCommand("no-such-project"); ok {
		t.Fatal("CardEventCommand: ok = true, want false (project never loaded)")
	}
}
