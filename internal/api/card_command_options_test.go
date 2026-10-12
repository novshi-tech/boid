package api

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCardCommandOptionsForProject_OrderedByDeclaration(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", &orchestrator.ProjectMeta{
		CardCommands: map[string]orchestrator.CardCommand{
			"zzz": {Label: "ZZZ", Run: "echo zzz"},
			"aaa": {Label: "AAA", Run: "echo aaa"},
			"mmm": {Label: "MMM", Run: "echo mmm"},
		},
		CardCommandsOrder: []string{"zzz", "aaa", "mmm"},
	})

	opts := svc.CardCommandOptionsForProject(context.Background(), "proj-1")

	wantKeys := []string{"zzz", "aaa", "mmm"}
	wantLabels := []string{"ZZZ", "AAA", "MMM"}
	if len(opts) != len(wantKeys) {
		t.Fatalf("opts = %+v, want %d entries", opts, len(wantKeys))
	}
	for i := range wantKeys {
		if opts[i].Key != wantKeys[i] || opts[i].Label != wantLabels[i] {
			t.Errorf("opts[%d] = %+v, want Key=%q Label=%q", i, opts[i], wantKeys[i], wantLabels[i])
		}
	}
}

func TestCardCommandOptionsForProject_NoCommandsDeclared_ReturnsNil(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", &orchestrator.ProjectMeta{})

	opts := svc.CardCommandOptionsForProject(context.Background(), "proj-1")
	if opts != nil {
		t.Errorf("opts = %+v, want nil for a project with no card_commands declared", opts)
	}
}

// TestCardCommandOptionsForProject_UnknownProject_ReturnsNil pins that a
// project id hydrateMetaForTriggers cannot resolve at all degrades to "no
// commands" rather than a panic — the Web UI must still render the rest of
// the card detail page.
func TestCardCommandOptionsForProject_UnknownProject_ReturnsNil(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", &orchestrator.ProjectMeta{
		CardCommands:      map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
		CardCommandsOrder: []string{"review"},
	})

	opts := svc.CardCommandOptionsForProject(context.Background(), "proj-does-not-exist")
	if opts != nil {
		t.Errorf("opts = %+v, want nil for an unresolvable project id", opts)
	}
}
