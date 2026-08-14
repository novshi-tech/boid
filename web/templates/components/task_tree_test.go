package components

import (
	"testing"

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
