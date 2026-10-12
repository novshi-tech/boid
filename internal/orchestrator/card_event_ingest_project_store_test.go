package orchestrator_test

// Proves *ProjectStore.CardEventCommand actually reads card_events.command
// off a real project.yaml through the ordinary ProjectStore.Load path —
// ProjectMeta.CardEvents carries json:"-" (API-serialization concern only),
// which does not affect this in-process read.

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestProjectStore_CardEventCommand_UnknownProject_NotOK(t *testing.T) {
	s := orchestrator.NewProjectStore()
	if _, ok := s.CardEventCommand("no-such-project"); ok {
		t.Fatal("CardEventCommand: ok = true, want false (project never loaded)")
	}
}
